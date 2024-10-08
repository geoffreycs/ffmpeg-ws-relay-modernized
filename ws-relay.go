// for build: go build -o ws-relay ws-relay.go
/*

usage:

ffmpeg -re -i v01.mp4 -c:v png -f image2pipe - | ./ws-relay -l :8081 -s png
ffmpeg -re -i v01.mp4 -s 1280x720 -c:v mjpeg -qscale:v 2 -f image2pipe - | ./ws-relay -l :8081

*/
package main

import (
	"errors"
	"flag"
	"log"
	"net/http"

	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os"

	ws "github.com/gorilla/websocket"
)

var localAddr = flag.String("l", ":8080", "")

var wsComp = flag.Bool("wscomp", false, "ws compression")
var verbosity = flag.Int("v", 3, "verbosity")

var queue = flag.Int("q", 1, "ws queue")

var split = flag.String("s", "jpg", "image type")

func alwaysTrue(r *http.Request) bool {
	return true
}

var upgrader = ws.Upgrader{CheckOrigin: alwaysTrue,
	EnableCompression: false} // use default options

var newclients chan *WsClient
var bufCh chan []byte

type WsClient struct {
	*ws.Conn
	data chan []byte
	die  bool
}

func NewWsClient(c *ws.Conn) *WsClient {
	return &WsClient{c, make(chan []byte, *queue), false}
}
func (c *WsClient) Send(buf []byte) error {
	if c.die {
		return errors.New("ws connection die")
	}

	select {
	case <-c.data:
	default:
	}
	c.data <- buf

	return nil
}
func (c *WsClient) worker() {
	for {
		buf := <-c.data
		//Vln(5, "[dbg]worker()", &c, len(buf))
		err := c.WriteMessage(ws.BinaryMessage, buf)
		if err != nil {
			c.Close()
			c.die = true
			return
		}
	}
}

func broacast() {
	clients := make(map[*WsClient]*WsClient, 0)

	for {
		data := <-bufCh
		//Vln(5, "[dbg]broacast()", len(data))
		for _, c := range clients {
			err := c.Send(data)
			if err != nil {
				delete(clients, c)
				Vln(3, "[ws][client]removed", c.RemoteAddr(), len(clients))
			}
		}
		for len(newclients) > 0 {
			c := <-newclients
			clients[c] = c
			Vln(3, "[ws][client]added", c.RemoteAddr())
		}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		Vln(2, "[ws]upgrade failed:", err)
		return
	}
	defer c.Close()

	Vln(3, "[ws][client]connect", c.RemoteAddr())
	client := NewWsClient(c)
	newclients <- client

	client.worker()

	Vln(3, "[ws][client]disconnect", c.RemoteAddr())
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	flag.Parse()

	upgrader.EnableCompression = *wsComp
	Vf(1, "ws EnableCompression = %v\n", *wsComp)
	Vf(1, "server Listen @ %v\n", *localAddr)
	Vf(1, "input image type = %v\n", *split)

	newclients = make(chan *WsClient, 16)
	bufCh = make(chan []byte, 1)
	go broacast()

	go connCam()

	http.HandleFunc("/ws", wsHandler)
	http.Handle("/", http.FileServer(http.Dir("./")))

	err := http.ListenAndServe(*localAddr, nil)
	if err != nil {
		Vln(1, "server listen error:", err)
	}
}

func connCam() {
	markMap := map[string](func(r *bufio.Reader, buf []byte) (int, error)){
		"jpg": readJPG,
		"png": readPNG,
	}

	decodeFn, ok := markMap[*split]
	if !ok {
		Vln(2, "[input][type]err:", *split)
		return
	}
	reader := bufio.NewReaderSize(os.Stdin, 8*1024*1024)
	buf := make([]byte, 8*1024*1024)
	//reader := os.Stdin
	for {
		n, err := decodeFn(reader, buf)
		if err != nil {
			Vln(2, "[pipe][recv]err:", err)
			return
		}

		Vln(5, "[dbg]connCam()", n, buf[:8])
		pack := make([]byte, n, n)
		copy(pack, buf[0:n])

		// broacast frame
		bufCh <- pack

		// do what you want with the frame
		// ...
	}
}

func readJPG(r *bufio.Reader, buf []byte) (int, error) {
	if n, err := io.ReadFull(r, buf[:2]); err != nil {
		return n, err
	}

	offset := 2
	if !bytes.Equal(buf[:2], []byte("\xFF\xD8")) {
		return offset, errors.New("may not JPG image")
	}

	for {
		s, err := r.ReadSlice('\xD9')
		if err != nil {
			return offset, err
		}

		copy(buf[offset:], s)
		offset += len(s)
		if bytes.HasSuffix(buf[:offset], []byte("\xFF\xD9")) {
			return offset, nil
		}
	}
}

func readPNG(r *bufio.Reader, buf []byte) (int, error) {
	if n, err := io.ReadFull(r, buf[:8]); err != nil {
		return n, err
	}

	offset := 8
	if !bytes.Equal(buf[:8], []byte("\x89PNG\x0D\x0A\x1A\x0A")) {
		return offset, errors.New("may not PNG image")
	}

	for {
		n, t, err := readPNGChunk(r, buf[offset:])
		offset += n
		if err != nil {
			return offset, err
		}
		if bytes.Equal(t, []byte("IEND")) {
			return offset, nil
		}
	}
}

func readPNGChunk(r io.Reader, buf []byte) (int, []byte, error) {
	if n, err := io.ReadFull(r, buf[:4]); err != nil { //chunk data length
		return n, nil, err
	}
	dataLen := binary.BigEndian.Uint32(buf[0:4]) + 4 + 4 // chunk type & CRC length

	n, err := io.ReadFull(r, buf[4:4+4]) // chunk type
	if err != nil {
		return n + 4, nil, err
	}
	chunkType := buf[4 : 4+4]

	n, err = io.ReadFull(r, buf[4+4:dataLen+4]) // chunk data & CRC
	if err != nil {
		return n + 8, chunkType, err
	}
	//Vln(6, "ReadPipe:", dataLen, string(chunkType))

	return n + 8, chunkType, nil
}

func Vln(level int, v ...interface{}) {
	if level <= *verbosity {
		log.Println(v...)
	}
}
func Vf(level int, format string, v ...interface{}) {
	if level <= *verbosity {
		log.Printf(format, v...)
	}
}
