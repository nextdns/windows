﻿using System;
using System.IO;
using System.IO.Pipes;
using System.Text;
using System.Diagnostics;
using System.Runtime.Serialization;
using System.Runtime.Serialization.Json;
using System.Threading.Tasks;

namespace NextDNS
{
    namespace Service
    {
        [DataContract]
        class Event
        {
            [DataMember(IsRequired=true)]
            public string name;

            [DataMember]
            public EventData data;

            public Event(string name)
            {
                this.name = name;
            }
        }
        [DataContract]
        class EventData
        {
            [DataMember]
            public bool enabled;

            [DataMember]
            public string error;

            [DataMember]
            public string configuration;

            [DataMember]
            public bool reportDeviceName;

            [DataMember]
            public bool checkUpdates;

            [DataMember]
            public string updateChannel;
        }
        class Client : IDisposable
        {
            private NamedPipeClientStream pipe;
            public event EventHandler<Event> EventReceived;
            public event EventHandler Connected;

            public void Dispose()
            {
                pipe.WaitForPipeDrain();
                pipe.Close();
                pipe.Dispose();
                pipe = null;
            }

            async public void Connect()
            {
                if (pipe != null)
                {
                    pipe.Close();
                    pipe.Dispose();
                }
                Debug.WriteLine("Connecting to service");
                pipe = new NamedPipeClientStream(".", "NextDNS", PipeDirection.InOut, PipeOptions.Asynchronous);
                while (true)
                {
                    try
                    {
                        await pipe.ConnectAsync(5000).ConfigureAwait(false);
                        break;
                    }
                    catch (Exception e)
                    {
                        Debug.WriteLine("NamedPipe connect: {0}", (object)e.Message);
                        pipe.Close();
                        await Task.Delay(5000).ConfigureAwait(false);
                    }
                }
                StartReadingAsync();
                Connected?.Invoke(this, EventArgs.Empty);
            }

            async public void StartReadingAsync()
            {
                var ser = new DataContractJsonSerializer(typeof(Event));
                using (var r = new StreamReader(pipe))
                {
                    string line;
                    while ((line = await r.ReadLineAsync()) != null)
                    {
                        using (var s = new MemoryStream(Encoding.UTF8.GetBytes(line)))
                        {
                            var e = (Event)ser.ReadObject(s);
                            Debug.WriteLine("Event received: {0}", (object)e.name);
                            EventReceived?.Invoke(this, e);
                        }
                    }
                    Debug.WriteLine("Pipe was closed");
                    Connect();
                }
            }

            public Task SendAsync(Event e)
            {
                Debug.WriteLine("Event sent: {0}", (object)e.name);
                var ms = new MemoryStream();
                var ser = new DataContractJsonSerializer(typeof(Event));
                ser.WriteObject(ms, e);
                ms.WriteByte(Convert.ToByte('\n'));
                byte[] json = ms.ToArray();
                ms.Close();
                return pipe.WriteAsync(json, 0, json.Length);
            }
        }
    }
}
