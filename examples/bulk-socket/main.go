package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ImVexed/iouring-go"
	"github.com/edsrzf/mmap-go"
	gsse "github.com/gin-contrib/sse"
	"github.com/r3labs/sse"
)

var (
	fds     = make([]int32, 0)
	message []byte
	ring    *iouring.Ring
	rmap    []byte
	rxCount *int32
	wg      = &sync.WaitGroup{}
)

func main() {
	rx := int32(0)
	rxCount = &rx

	msg := struct {
		ID      int
		Author  string
		Content string
	}{
		112,
		"Sample User",
		"Sample message 123",
	}

	// Create a static message to deliver that can also be compared against when we receive it over the network
	message, _ = json.Marshal(msg)

	ring, _ = iouring.New(1024, &iouring.Params{})

	rmap, _ = mmap.MapRegion(nil, 10<<10, mmap.RDWR, mmap.ANON, 0)

	vecs := []*syscall.Iovec{
		{
			Base: &rmap[0],
			Len:  uint64(len(rmap)),
		},
	}

	iouring.RegisterBuffers(ring.Fd(), vecs)

	// Start a new go routine that sends a message every second
	go sendMessage()

	lck := &sync.Mutex{}
	// Create a SSE endpoint that hijacks all incoming connections and adds their underlying file descriptors to an array
	http.HandleFunc("/listen", func(w http.ResponseWriter, r *http.Request) {
		lck.Lock()
		defer lck.Unlock()

		nc, _, err := w.(http.Hijacker).Hijack()

		if err != nil {
			log.Fatalln(err.Error())
		}

		// nc.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))

		if _, err := nc.Write([]byte("HTTP/1.0 200 OK\r\nConnection: keep-alive\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")); err != nil {
			log.Fatalln(err.Error())
		}

		sf, err := nc.(*net.TCPConn).File()

		if err != nil {
			log.Fatalln(err.Error())
		}

		fds = append(fds, int32(sf.Fd()))
	})

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}

	addr := fmt.Sprintf("http://localhost:%d/listen", l.Addr().(*net.TCPAddr).Port)

	go http.Serve(l, nil)

	// Spawn n many clients to establish an SSE
	for i := 0; i < 100; i++ {
		go spawnClient(addr)
	}

	select {}
}

type backOff struct{}

func (b *backOff) NextBackOff() time.Duration { return -1 }
func (b *backOff) Reset()                     {}

func spawnClient(addr string) {
	c := sse.NewClient(addr)
	c.ReconnectStrategy = &backOff{}

	// Subscribe to the SSE endpoint
	if err := c.Subscribe("", func(evt *sse.Event) {
		// If we receive an event that isn't equal to our preset message, it has been corrupted
		if string(message) != string(evt.Data) {
			log.Fatalf("Client received invalid response, expected: %s but got %s", string(message), string(evt.Data))
		}
		wg.Done()
		// fmt.Println("Inc")
	}); err != nil {
		log.Fatalln("Subscribe failed", err.Error())
	}
}

func sendMessage() {
	for {
		time.Sleep(1 * time.Second)

		if err := send(fds, message); err != nil {
			log.Fatal(err.Error())
		}
	}
}

func send(fds []int32, data []byte) error {
	start := time.Now()

	var b bytes.Buffer
	// Encode the JSON message into an SSE
	if err := gsse.Encode(&b, gsse.Event{
		Event: "message",
		Data:  json.RawMessage(data),
	}); err != nil {
		return err
	}

	sdata := b.Bytes()

	wire := bytes.Buffer{}

	// Wrap the SSE into the chunked http wire format
	fmt.Fprintf(&wire, "%x\r\n", len(sdata))
	wire.Write(sdata)
	wire.WriteString("\r\n")

	rawData := wire.Bytes()
	copy(rmap, rawData)

	addr := (uint64)(uintptr(unsafe.Pointer(&rmap[0])))
	length := uint32(len(rawData))

	// Queue up n many SQE's for each file descriptor
	for _, fd := range fds {
		e, commit := ring.SubmitEntry()

		e.Opcode = iouring.WriteFixed
		e.Fd = fd
		e.Addr = addr
		e.Len = length

		commit()
	}

	wg.Add(len(fds))

	res := ring.Enter(uint(len(fds)), uint(len(fds)), iouring.EnterGetEvents, nil)

	fmt.Println("Waiting")
	wg.Wait()

	tail := atomic.LoadUint32(ring.Cq.Tail)
	mask := atomic.LoadUint32(ring.Cq.Mask)
	atomic.StoreUint32(ring.Cq.Head, tail&mask)

	// seenIdx := uint32(0)
	// seen := false
	// seenEnd := false
	// for i := uint32(0); i <= tail&mask; i++ {
	// 	if ring.Cq.Entries[i].Flags&1 == 1 {
	// 		seen = true
	// 	} else if !seenEnd {
	// 		seen = false
	// 		seenEnd = true
	// 	}
	// 	if seen == true && !seenEnd {
	// 		seenIdx = i
	// 	}

	// 	ring.Cq.Entries[i].Flags |= 1
	// 	atomic.StoreUint32(ring.Cq.Head, seenIdx)
	// }

	fmt.Println("Sq Head", atomic.LoadUint32(ring.Sq.Head))
	fmt.Println("Sq Tail", atomic.LoadUint32(ring.Sq.Tail))
	fmt.Println("Sq Mask", atomic.LoadUint32(ring.Sq.Mask))
	fmt.Println("Sq Dropped", atomic.LoadUint32(ring.Sq.Dropped))

	fmt.Println("Cq Head", atomic.LoadUint32(ring.Cq.Head))
	fmt.Println("Cq Tail", atomic.LoadUint32(ring.Cq.Tail))
	fmt.Println("Cq Mask", atomic.LoadUint32(ring.Cq.Mask))

	// ring.Sq.Reset()
	// atomic.StoreUint32(ring.Sq.Head, 100)
	// atomic.StoreUint32(ring.Sq.Tail, 100)

	// tail := atomic.LoadUint32(ring.Cq.Tail)
	// mask := atomic.LoadUint32(ring.Cq.Mask)

	// for i := uint32(0); i <= tail&mask; i++ {
	// 	ring.Cq.Entries[i].Flags |= 1
	// }

	//atomic.StoreUint32(ring.Cq.Head, atomic.LoadUint32(ring.Cq.Head)+uint32(len(fds)))
	//atomic.StoreUint32(ring.Cq.Tail, 0)

	fmt.Printf("Sent %d bytes to %d sockets in %s.\n", len(data), len(fds), time.Now().Sub(start).String())
	return res
}
