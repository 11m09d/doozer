package doozer

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"io"
	"os"
	pb "goprotobuf.googlecode.com/hg/proto"
	"sync"
)

const (
	Valid = 1 << iota
	Done
	Set
	Del
)


var (
	ErrNoAddrs = os.NewError("no known address")
	ErrBadTag  = os.NewError("bad tag")
	ErrOldRev  = os.NewError("old rev given")
)

var (
	cancel = NewRequest_Verb(Request_CANCEL)
	del    = NewRequest_Verb(Request_DEL)
	get    = NewRequest_Verb(Request_GET)
	nop    = NewRequest_Verb(Request_NOP)
	rev    = NewRequest_Verb(Request_REV)
	set    = NewRequest_Verb(Request_SET)
	walk   = NewRequest_Verb(Request_WALK)
	watch  = NewRequest_Verb(Request_WATCH)
	stat   = NewRequest_Verb(Request_STAT)
	getdir = NewRequest_Verb(Request_GETDIR)
)


type Error struct {
	Code   int32
	Detail string
}


func (r *Error) String() (s string) {
	s = "response: " + Response_Err_name[r.Code]
	if r.Detail != "" {
		s += ": " + r.Detail
	}
	return s
}


var errs = map[int32]os.Error{
	Response_NOTDIR:       os.ENOTDIR,
	Response_ISDIR:        os.EISDIR,
	Response_NOENT:        os.ENOENT,
	Response_REV_MISMATCH: ErrOldRev,
	Response_TOO_LATE:     os.NewError("too late"),
}


type Event struct {
	Rev  int64
	Path string
	Body []byte
	Flag int32
	Err  os.Error
}


func (e Event) IsSet() bool {
	return e.Flag&Set > 0
}


func (e Event) IsDel() bool {
	return e.Flag&Del > 0
}


type T Request

type R Response


func (r *R) err() os.Error {
	if r == nil {
		return os.EOF
	}

	if r.ErrCode != nil {
		c := int32(*r.ErrCode)

		if r.ErrDetail != nil {
			return &Error{c, *r.ErrDetail}
		}

		if r, ok := errs[c]; ok {
			return r
		}

		return &Error{c, ""}
	}
	return nil
}


func (r *R) String() string {
	return fmt.Sprintf("%#v", r)
}


type conn struct {
	addr string
	c    net.Conn
	clk  sync.Mutex // write lock
	err  os.Error   // unrecoverable error

	// callback management
	n    int32             // next tag
	cb   map[int32]chan *R // callback channels
	cblk sync.Mutex

	// redirect handling
	redirectAddr string
	redirected   bool

	closed chan bool
}


func (c *conn) writeT(t *T) os.Error {
	if c.err != nil {
		return c.err
	}

	buf, err := pb.Marshal(t)
	if err != nil {
		return err
	}

	c.err = binary.Write(c.c, binary.BigEndian, int32(len(buf)))
	if c.err != nil {
		return c.err
	}

	for len(buf) > 0 {
		n, err := c.c.Write(buf)
		if err != nil {
			c.err = err
			return err
		}

		buf = buf[n:]
	}

	return nil
}


func (c *conn) send(t *T) (chan *R, os.Error) {
	if c.err != nil {
		return nil, c.err
	}

	ch := make(chan *R)

	// Find an unused tag and
	// put the reply chan in the table.
	c.cblk.Lock()
	for _, ok := c.cb[c.n]; ok; _, ok = c.cb[c.n] {
		c.n++
	}
	tag := c.n
	c.cb[tag] = ch
	c.cblk.Unlock()

	t.Tag = &tag

	c.clk.Lock()
	err := c.writeT(t)
	c.clk.Unlock()

	if err != nil {
		c.cblk.Lock()
		c.cb[tag] = nil, false
		c.cblk.Unlock()
		return nil, err
	}

	return ch, nil
}


func (c *conn) call(t *T) (*R, os.Error) {
	ch, err := c.send(t)
	if err != nil {
		return nil, err
	}

	r := <-ch
	if err := r.err(); err != nil {
		return nil, err
	}

	return r, nil
}


func (c *conn) events(t *T) (*Watch, os.Error) {
	cb, err := c.send(t)
	if err != nil {
		return nil, err
	}

	evs := make(chan *Event)
	w := &Watch{evs, c, cb, *t.Tag}
	go func() {
		for r := range cb {
			var ev Event
			if err := r.err(); err != nil {
				ev.Err = err
			} else {
				ev.Rev = pb.GetInt64(r.Rev)
				ev.Path = pb.GetString(r.Path)
				ev.Body = r.Value
				ev.Flag = pb.GetInt32(r.Flags)
			}
			evs <- &ev
		}
		close(evs)
	}()

	return w, nil
}


func (c *conn) readR() (*R, os.Error) {
	var size int32
	err := binary.Read(c.c, binary.BigEndian, &size)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, size)
	_, err = io.ReadFull(c.c, buf)
	if err != nil {
		return nil, err
	}

	var r R
	err = pb.Unmarshal(buf, &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}


func (c *conn) close() {
	c.cblk.Lock()
	for _, ch := range c.cb {
		if ch != nil {
			close(ch)
		}
	}
	c.cb = nil
	c.cblk.Unlock()
	c.closed <- true
}


func (c *conn) readResponses() {
	defer c.close()

	for {
		r, err := c.readR()
		if err != nil {
			c.clk.Lock()
			if c.err == nil {
				c.err = err
			}
			c.clk.Unlock()
			return
		}

		if r.ErrCode != nil && *r.ErrCode == Response_REDIRECT {
			c.redirectAddr = pb.GetString(r.ErrDetail)
			c.redirected = true
		}

		tag := pb.GetInt32(r.Tag)
		flags := pb.GetInt32(r.Flags)

		c.cblk.Lock()
		ch, ok := c.cb[tag]
		if ok && ch == nil {
			c.cblk.Unlock()
			continue
		}
		if flags&Done != 0 {
			c.cb[tag] = nil, false
		}
		c.cblk.Unlock()

		if !ok {
			log.Printf(
				"%v unexpected: tag=%d flags=%d rev=%d path=%q value=%v len=%d err_code=%v err_detail=%q",
				ch,
				tag,
				flags,
				pb.GetInt64(r.Rev),
				pb.GetString(r.Path),
				r.Value,
				pb.GetInt32(r.Len),
				pb.GetInt32((*int32)(r.ErrCode)),
				pb.GetString(r.ErrDetail),
			)
			continue
		}

		if flags&Valid != 0 {
			ch <- r
		}

		if flags&Done != 0 {
			close(ch)
		}
	}
}


func (c *conn) cancel(tag int32, cb chan *R) os.Error {
	c.cblk.Lock()
	ch, ok := c.cb[tag]
	if !ok || ch != cb {
		c.cblk.Unlock()
		return ErrBadTag
	}

	// Make a nil entry, to prevent any goroutine from
	// reusing this tag until we are done with it.
	c.cb[tag] = nil
	c.cblk.Unlock()

	_, err := c.call(&T{Verb: cancel, OtherTag: &tag})
	if err != nil {
		// Something is very wrong.
		// Leave a nil entry in the cb map,
		// so we don't reuse this tag.
		return err
	}

	c.cblk.Lock()
	// Remove our nil entry, freeing up this tag for reuse.
	c.cb[tag] = nil, false
	c.cblk.Unlock()

	close(ch)
	return nil
}


func (c *conn) monitorAddrs(cl *Client) {
	addrs := make(map[string]string)
	addAddr := func(p, a string) {
		if len(a) > 0 {
			addrs[p] = a
		}
	}

	addrGlob := pb.String("/ctl/node/*/addr")
	watchAddr, err := c.events(&T{Verb: watch, Path: addrGlob})
	if err != nil {
		log.Println(err)
		return
	}

	walkAddr, err := c.events(&T{Verb: walk, Path: addrGlob})
	if err != nil {
		log.Println(err)
		return
	}

init:
	for {
		select {
		case ev := <-walkAddr.C:
			if closed(walkAddr.C) {
				break init
			}
			addAddr(ev.Path, string(ev.Body))
		case ev := <-watchAddr.C:
			addAddr(ev.Path, string(ev.Body))
		}
	}

	glob := pb.String("/ctl/cal/*")

	watch, err := c.events(&T{Verb: watch, Path: glob})
	if err != nil {
		log.Println(err)
		return
	}

	walk, err := c.events(&T{Verb: walk, Path: glob})
	if err != nil {
		log.Println(err)
		return
	}

	cal := make(map[string]string)

	for {
		if watchAddr.C == nil && walk.C == nil && watch.C == nil {
			break
		}

		select {
		case ev := <-watchAddr.C:
			if closed(watchAddr.C) {
				watchAddr.C = nil
				break
			}
			addAddr(ev.Path, string(ev.Body))
		case ev := <-walk.C:
			if closed(walk.C) {
				walk.C = nil
				break
			}

			if len(ev.Body) > 0 {
				sid := string(ev.Body)
				path := "/ctl/node/" + sid + "/addr"
				addr := addrs[path]
				cal[ev.Path] = addr
				cl.a <- addr
			} else {
				addr := cal[ev.Path]
				cal[ev.Path] = "", false
				cl.r <- addr
			}
		case ev := <-watch.C:
			if closed(watch.C) {
				watch.C = nil
				break
			}

			if len(ev.Body) > 0 {
				sid := string(ev.Body)
				path := "/ctl/node/" + sid + "/addr"
				addr := addrs[path]
				cal[ev.Path] = addr
				cl.a <- addr
			} else {
				addr := cal[ev.Path]
				cal[ev.Path] = "", false
				cl.r <- addr
			}
		}
	}
}


type Client struct {
	Name string
	c    chan *conn  // current connection
	a    chan string // add address
	r    chan string // remove address
	Len  chan int
}


// Name is the name of this cluster.
// Addr is an initial (writable) address to connect to.
func New(name, addr string) *Client {
	c := &Client{
		Name: name,
		c:    make(chan *conn),
		a:    make(chan string),
		r:    make(chan string),
		Len:  make(chan int),
	}
	go c.run(map[string]bool{addr: true})
	return c
}


func (cl *Client) connect(a map[string]bool) *conn {
	for len(a) > 0 {
		var addr string
		for addr = range a {
			break
		}
		c, err := cl.dial(addr)
		if err == nil {
			return c
		}
		log.Println(err)
		a[addr] = false, false
	}
	close(cl.c)
	return nil
}


func (cl *Client) run(a map[string]bool) {
	c := cl.connect(a)
	if c == nil {
		return
	}

	for {
		select {
		case cl.Len <- len(a):
			// nothing
		case cl.c <- c:
			// nothing
		case add := <-cl.a:
			a[add] = true
		case rm := <-cl.r:
			a[rm] = false, false
		case <-c.closed:
			a[c.addr] = false, false
			c = cl.connect(a)
			if c == nil {
				return
			}
		}
	}
}


func (cl *Client) dial(addr string) (*conn, os.Error) {
	var c conn
	var err os.Error

	c.addr = addr
	c.c, err = net.Dial("tcp", "", addr)
	if err != nil {
		return nil, err
	}

	c.cb = make(map[int32]chan *R)
	c.closed = make(chan bool, 1)
	go c.readResponses()
	go c.monitorAddrs(cl)
	return &c, nil
}


func (cl *Client) call(t *T) (r *R, err os.Error) {
	c := <-cl.c
	if c == nil {
		return nil, ErrNoAddrs
	}

	return c.call(t)
}


func (cl *Client) retry(t *T) (r *R, err os.Error) {
	for {
		c := <-cl.c
		if c == nil {
			return nil, ErrNoAddrs
		}

		r, err = c.call(t)
		if c.err != nil {
			// connection error? then try again with a new conn
			continue
		}

		// success, or some other error
		return
	}

	panic("not reached")
}


func (cl *Client) Set(path string, oldRev int64, body []byte) (newRev int64, err os.Error) {
	r, err := cl.call(&T{Verb: set, Path: &path, Value: body, Rev: &oldRev})
	if err != nil {
		return 0, err
	}

	return pb.GetInt64(r.Rev), nil
}


// Returns the body and revision of the file at path.
// If rev is 0, uses the current state, otherwise,
// rev must be a value previously returned buy an operation.
// If path does not denote a file, returns an error.
func (cl *Client) Get(path string, rev *int64) ([]byte, int64, os.Error) {
	r, err := cl.retry(&T{Verb: get, Path: &path, Rev: rev})
	if err != nil {
		return nil, 0, err
	}

	return r.Value, pb.GetInt64(r.Rev), nil
}


func (cl *Client) Rev() (int64, os.Error) {
	r, err := cl.retry(&T{Verb: rev})
	if err != nil {
		return 0, err
	}

	return *r.Rev, nil
}


func (cl *Client) Del(path string, rev int64) os.Error {
	_, err := cl.call(&T{Verb: del, Path: &path, Rev: &rev})
	return err
}

func (cl *Client) Stat(path string, rev *int64) (int32, int64, os.Error) {
	r, err := cl.retry(&T{Verb: stat, Path: &path, Rev: rev})
	if err != nil {
		return 0, 0, err
	}

	return pb.GetInt32(r.Len), pb.GetInt64(r.Rev), nil
}

func (cl *Client) Nop() os.Error {
	_, err := cl.call(&T{Verb: nop})
	return err
}


func (cl *Client) Watch(glob string, from int64) (*Watch, os.Error) {
	c := <-cl.c
	if c == nil {
		return nil, ErrNoAddrs
	}

	return c.events(&T{Verb: watch, Path: &glob, Rev: &from})
}

func (cl *Client) Getdir(path string, offset, limit int32, rev *int64) (*Watch, os.Error) {
	c := <-cl.c
	if c == nil {
		return nil, ErrNoAddrs
	}

	var t T
	t.Verb = getdir
	t.Rev = rev
	t.Path = &path
	t.Offset = &offset
	t.Limit = &limit

	return c.events(&t)
}

func (cl *Client) Walk(glob string, rev *int64, offset, limit *int32) (*Watch, os.Error) {
	c := <-cl.c
	if c == nil {
		return nil, ErrNoAddrs
	}

	return c.events(&T{
		Verb:   walk,
		Path:   &glob,
		Rev:    rev,
		Offset: offset,
		Limit:  limit,
	})
}


type Watch struct {
	C   <-chan *Event // to caller
	c   *conn
	cb  chan *R
	tag int32
}


func (w *Watch) Cancel() os.Error {
	return w.c.cancel(w.tag, w.cb)
}
