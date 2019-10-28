package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	tun "github.com/rs/nextdns-windows/tun"
)

type Proxy struct {
	Upstream string

	ExtraHeaders http.Header

	OnStateChange func(started bool)

	// Transport is the http.RoundTripper used to perform DoH requests.
	Transport http.RoundTripper

	// QueryLog specifies an optional log function called for each received query.
	QueryLog func(msgID uint16, qname string)

	// ErrorLog specifies an optional log function for errors. If not set,
	// errors are not reported.
	ErrorLog func(error)

	InfoLog func(string)

	tun  io.ReadWriteCloser
	stop chan struct{}

	dedup dedup
}

func (p *Proxy) Started() bool {
	return p.tun != nil
}

func (p *Proxy) Start() (err error) {
	if p.tun != nil {
		return
	}
	if p.tun, err = tun.OpenTunDevice("tun0", "192.0.2.43", "192.0.2.42", "255.255.255.0", []string{"192.0.2.42"}); err != nil {
		return err
	}
	go p.run()
	return nil
}

func (p *Proxy) Stop() (err error) {
	if p.tun != nil {
		err = p.tun.Close()
		p.tun = nil
	}
	if p.stop != nil {
		close(p.stop)
	}
	return err
}

func (p *Proxy) logQuery(msgID uint16, qname string) {
	if p.QueryLog != nil {
		p.QueryLog(msgID, qname)
	}
}

func (p *Proxy) logInfo(msg string) {
	if p.InfoLog != nil {
		p.InfoLog(msg)
	}
}
func (p *Proxy) logErr(err error) {
	if err != nil && p.ErrorLog != nil {
		p.ErrorLog(err)
	}
}

func (p *Proxy) run() {
	if p.OnStateChange != nil {
		p.OnStateChange(true)
	}
	defer func() {
		p.tun = nil
		if p.OnStateChange != nil {
			p.OnStateChange(false)
		}
	}()

	// Setup firewall rules to avoid DNS leaking.
	// The process block forever and removes rules when killed.
	// We thus kill it as soon as we stop the proxy.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.unleak(ctx); err != nil {
		p.logErr(fmt.Errorf("cannot start dnsunleak: %v", err))
	}

	// Start the loop handling UDP packets received on the tun interface.
	const maxSize = 1500
	bpool := sync.Pool{
		New: func() interface{} {
			b := make([]byte, maxSize)
			return &b
		},
	}
	p.stop = make(chan struct{})
	// Isolate the reads in a goroutine so we can decide to bail when p.stop is
	// closed, even if tun.Read keeps blocking. This is to make sure we stop
	// dnsunleak and not leave the user with no DNS. This certainly hides a bug
	// in the tun library.
	packetIn := make(chan []byte)
	packetOut := make(chan []byte)
	tun := p.tun
	go func() {
		defer close(packetIn)
		for {
			buf := *bpool.Get().(*[]byte)
			n, err := tun.Read(buf[:maxSize]) // make sure we resize it to its max size
			if err != nil {
				if err != io.EOF {
					p.logErr(fmt.Errorf("tun read err: %v", err))
				}
				return
			}
			packetIn <- buf[:n]
		}
	}()
	go func() {
		for {
			var buf []byte
			var more bool
			select {
			case buf, more = <-packetOut:
				if !more {
					return
				}
			case <-p.stop:
				return
			}
			if _, err := tun.Write(buf); err != nil {
				p.logErr(fmt.Errorf("tun write error: %v", err))
				return
			}
			bpool.Put(&buf)
		}
	}()

	dnsIP := []byte{192, 0, 2, 42}
	for {
		var buf []byte
		select {
		case buf = <-packetIn:
		case <-p.stop:
			return
		}
		qsize := len(buf)
		if qsize <= 20 {
			bpool.Put(&buf)
			continue
		}
		if buf[9] != 17 {
			// Not UDP
			bpool.Put(&buf)
			continue
		}
		if !bytes.Equal(buf[16:20], dnsIP) {
			// Skip packet not directed to us.
			bpool.Put(&buf)
			continue
		}
		msgID := lazyMsgID(buf)
		if p.dedup.IsDup(msgID) {
			bpool.Put(&buf)
			// Skip duplicated query.
			continue
		}
		go func() {
			qname := lazyQName(buf)
			p.logQuery(msgID, qname)
			res, err := p.resolve(buf)
			if err != nil {
				p.logErr(fmt.Errorf("resolve: %x %v", msgID, err))
				return
			}
			buf = buf[:maxSize] // reset buf size to it's underlaying size
			rsize, err := readDNSResponse(res, buf)
			if err != nil {
				p.logErr(fmt.Errorf("readDNSResponse: %v", err))
				return
			}
			select {
			case packetOut <- buf[:rsize]:
			case <-p.stop:
			}
		}()
	}
}

func (p *Proxy) unleak(ctx context.Context) error {
	// Setup firewall rules to avoid DNS leaking.
	// The process block forever and removes rules when killed.
	// We thus kill it as soon as we stop the proxy.
	ex, _ := os.Executable()
	dnsunleakPath := filepath.Join(filepath.Dir(ex), "dnsunleak.exe")
	cmd := exec.CommandContext(ctx, dnsunleakPath)
	stdout, stdoutW := io.Pipe()
	stdinR, stdin := io.Pipe()
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			l := s.Text()
			p.logInfo(fmt.Sprintf("dnsunleak: %s", l))
		}
	}()
	go func() {
		<-ctx.Done()
		if proc := cmd.Process; proc != nil {
			p.logInfo("Killing dnsunleak")
			_, _ = stdin.Write([]byte{'\n'})
			_ = proc.Kill()
		}
	}()
	return cmd.Start()
}

func (p *Proxy) resolve(buf []byte) (io.ReadCloser, error) {
	req, err := http.NewRequest("POST", p.Upstream, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-packet")
	for name, hdrs := range p.ExtraHeaders {
		req.Header[name] = hdrs
	}
	rt := p.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", res.StatusCode)
	}
	return res.Body, nil
}

func readDNSResponse(r io.Reader, buf []byte) (int, error) {
	var n int
	for {
		nn, err := r.Read(buf[n:])
		n += nn
		if err != nil {
			if err == io.EOF {
				break
			}
			return -1, err
		}
		if n >= len(buf) {
			buf[2] |= 0x2 // mark response as truncated
			break
		}
	}
	return n, nil
}

// lazyMsgID parses the message ID from a DNS query wything trying to parse or
// validate the whole query.
func lazyMsgID(buf []byte) uint16 {
	if len(buf) < 30 {
		return 0
	}
	return uint16(buf[28])<<8 | uint16(buf[29])
}

// lazyQName parses the qname from a DNS query without trying to parse or
// validate the whole query.
func lazyQName(buf []byte) string {
	qn := &strings.Builder{}
	for n := 40; n <= len(buf) && buf[n] != 0; {
		end := n + 1 + int(buf[n])
		if end > len(buf) {
			// invalid qname, stop parsing
			break
		}
		qn.Write(buf[n+1 : end])
		qn.WriteByte('.')
		n = end
	}
	return qn.String()
}
