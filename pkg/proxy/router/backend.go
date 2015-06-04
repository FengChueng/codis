package router

import (
	"sync"
	"time"

	"github.com/wandoulabs/codis/pkg/proxy/redis"
	"github.com/wandoulabs/codis/pkg/utils/log"
)

type BackendConn struct {
	Addr string
	stop sync.Once

	input chan *Request
}

func NewBackendConn(addr string) *BackendConn {
	bc := &BackendConn{
		Addr:  addr,
		input: make(chan *Request, 1024),
	}
	go bc.Run()
	return bc
}

func (bc *BackendConn) Run() {
	log.Infof("backend conn [%p] to %s, start service", bc, bc.Addr)
	for k := 0; ; k++ {
		starttime := time.Now()
		err := bc.loopWriter()
		if err == nil {
			break
		}
		var n int
		if time.Since(starttime) < time.Second {
			n = bc.discard(err, len(bc.input)/20+1)
		}
		if n != 0 {
			log.InfoErrorf(err, "backend conn [%p] to %s, restart [%d], discard next %d requests",
				bc, bc.Addr, k, n)
		} else {
			log.InfoErrorf(err, "backend conn [%p] to %s, restart [%d]",
				bc, bc.Addr, k)
		}
		time.Sleep(time.Millisecond * 50)
	}
	log.Infof("backend conn [%p] to %s, stop and exit", bc, bc.Addr)
}

func (bc *BackendConn) Close() {
	bc.stop.Do(func() {
		close(bc.input)
	})
}

func (bc *BackendConn) PushBack(r *Request) {
	bc.input <- r
}

func (bc *BackendConn) discard(err error, max int) int {
	var n int
	for i := 0; i < max; i++ {
		select {
		case r, ok := <-bc.input:
			if !ok {
				return n
			}
			bc.setResponse(r, nil, err)
			n++
		default:
		}
	}
	return n
}

func (bc *BackendConn) loopWriter() error {
	r, ok := <-bc.input
	if ok {
		c, tasks, err := bc.newBackendReader()
		if err != nil {
			return bc.setResponse(r, nil, err)
		}
		defer close(tasks)

		var nbuffered int
		var lastflush time.Time
		for ok {
			var flush bool
			if len(bc.input) == 0 {
				flush = true
			} else if nbuffered >= 64 || nbuffered >= len(tasks) {
				flush = true
			} else if time.Since(lastflush) >= time.Microsecond*300 {
				flush = true
			}

			if err := c.Writer.Encode(r.Resp, flush); err != nil {
				c.Close()
				return bc.setResponse(r, nil, err)
			}
			tasks <- r

			if flush {
				nbuffered, lastflush = 0, time.Now()
			} else {
				nbuffered++
			}

			r, ok = <-bc.input
		}
	}
	return nil
}

func (bc *BackendConn) newBackendReader() (*redis.Conn, chan<- *Request, error) {
	c, err := redis.DialTimeout(bc.Addr, 1024*512, time.Second)
	if err != nil {
		return nil, nil, err
	}
	c.ReaderTimeout = time.Minute
	c.WriterTimeout = time.Minute

	tasks := make(chan *Request, 4096)
	go func() {
		defer c.Close()
		for r := range tasks {
			resp, err := c.Reader.Decode()
			bc.setResponse(r, resp, err)
		}
	}()
	return c, tasks, nil
}

func (bc *BackendConn) setResponse(r *Request, resp *redis.Resp, err error) error {
	r.Response.Resp, r.Response.Err = resp, err
	r.wait.Done()
	r.slot.jobs.Done()
	return err
}
