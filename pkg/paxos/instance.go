package paxos

type instance struct {
	vin     chan string
	v       string
	done    chan int
	cPutter putCloser // Coordinator
	aPutter putCloser // Acceptor
	lPutter putCloser // Learner
	cx      *cluster
	cxReady chan int
}

func newInstance(cxf func() *cluster, outs Putter) *instance {
	c, aIns, lIns := newCoord(outs), make(chanPutCloser), make(chanPutCloser)
	ins := &instance{
		vin:     make(chan string),
		done:    make(chan int),
		cPutter: c,
		aPutter: aIns,
		lPutter: lIns,
		cxReady: make(chan int),
	}

	go func() {
		ins.cx = cxf()
		close(ins.cxReady)
		go c.process(ins.cx)
		go acceptor(aIns, outs)
		go func() {
			ins.v = learner(uint64(ins.cx.Quorum()), lIns)
			close(ins.done)
		}()
	}()

	return ins
}

func (it *instance) cluster() *cluster {
	<-it.cxReady
	return it.cx
}

func (ins *instance) Put(m Msg) {
	ins.cPutter.Put(m)
	ins.aPutter.Put(m)
	ins.lPutter.Put(m)
}

func (ins *instance) Value() string {
	<-ins.done
	return ins.v
}

func (ins *instance) Close() {
	ins.cPutter.Close()
	ins.aPutter.Close()
	ins.lPutter.Close()
}

func (ins *instance) Propose(v string) {
	ins.cPutter.Put(newPropose(v))
}
