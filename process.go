package ergonode

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/halturin/ergonode/etf"
	"github.com/halturin/ergonode/lib"
)

type ProcessType = string

const (
	DefaultProcessMailboxSize = 100
)

type Process struct {
	sync.RWMutex

	mailBox      chan etf.Tuple
	ready        chan bool
	gracefulExit chan gracefulExitRequest
	self         etf.Pid
	groupLeader  *Process
	Context      context.Context
	Kill         context.CancelFunc
	Exit         ProcessExitFunc
	name         string
	Node         *Node

	object interface{}
	state  interface{}
	reply  chan etf.Tuple

	env map[string]interface{}

	parent          *Process
	reductions      uint64 // we use this term to count total number of processed messages from mailBox
	currentFunction string

	trapExit bool
}

type gracefulExitRequest struct {
	from   etf.Pid
	reason string
}
type ProcessInfo struct {
	CurrentFunction string
	Status          string
	MessageQueueLen int
	Links           []etf.Pid
	Dictionary      etf.Map
	TrapExit        bool
	GroupLeader     etf.Pid
	Reductions      uint64
}

type ProcessOptions struct {
	MailboxSize uint16
	GroupLeader *Process
	parent      *Process
}

type ProcessExitFunc func(from etf.Pid, reason string)

// Behaviour interface contains methods you should implement to make own process behaviour
type ProcessBehaviour interface {
	loop(*Process, interface{}, ...interface{}) string // method which implements control flow of process
}

// Self returns self Pid
func (p *Process) Self() etf.Pid {
	return p.self
}

// Name returns registered name of the process
func (p *Process) Name() string {
	return p.name
}

// Call makes outgoing sync request in fashion of 'gen_call'.
// 'to' can be Pid, registered local name or a tuple {RegisteredName, NodeName}
func (p *Process) Call(to interface{}, message etf.Term) (etf.Term, error) {
	return p.CallWithTimeout(to, message, DefaultCallTimeout)
}

// CallWithTimeout makes outgoing sync request in fashiod of 'gen_call' with given timeout
func (p *Process) CallWithTimeout(to interface{}, message etf.Term, timeout int) (etf.Term, error) {
	ref := p.Node.MakeRef()
	from := etf.Tuple{p.self, ref}
	msg := etf.Term(etf.Tuple{etf.Atom("$gen_call"), from, message})
	p.Send(to, msg)
	for {
		select {
		case m := <-p.reply:
			ref1 := m[0].(etf.Ref)
			val := m[1].(etf.Term)
			// check message Ref
			if len(ref.Id) == 3 && ref.Id[0] == ref1.Id[0] && ref.Id[1] == ref1.Id[1] && ref.Id[2] == ref1.Id[2] {
				return val, nil
			}
			// ignore this message. waiting for the next one
		case <-time.After(time.Second * time.Duration(timeout)):
			return nil, errors.New("timeout")
		case <-p.Context.Done():
			return nil, errors.New("stopped")
		}
	}
}

// CallRPC evaluate rpc call with given node/MFA
func (p *Process) CallRPC(node, module, function string, args ...etf.Term) (etf.Term, error) {
	return p.CallRPCWithTimeout(DefaultCallTimeout, node, module, function, args...)
}

// CallRPCWithTimeout evaluate rpc call with given node/MFA and timeout
func (p *Process) CallRPCWithTimeout(timeout int, node, module, function string, args ...etf.Term) (etf.Term, error) {
	lib.Log("[%s] RPC calling: %s:%s:%s", p.Node.FullName, node, module, function)
	message := etf.Tuple{
		etf.Atom("call"),
		etf.Atom(module),
		etf.Atom(function),
		etf.List(args),
	}
	to := etf.Tuple{etf.Atom("rex"), etf.Atom(node)}
	return p.CallWithTimeout(to, message, timeout)
}

// CastRPC evaluate rpc cast with given node/MFA
func (p *Process) CastRPC(node, module, function string, args ...etf.Term) {
	lib.Log("[%s] RPC casting: %s:%s:%s", p.Node.FullName, node, module, function)
	message := etf.Tuple{
		etf.Atom("cast"),
		etf.Atom(module),
		etf.Atom(function),
		etf.List(args),
	}
	to := etf.Tuple{etf.Atom("rex"), etf.Atom(node)}
	p.Cast(to, message)
}

// Send sends a message. 'to' can be Pid, registered local name
// or a tuple {RegisteredName, NodeName}
func (p *Process) Send(to interface{}, message etf.Term) {
	p.Node.registrar.route(p.self, to, message)
}

// Cast sends a message in fashion of 'gen_cast'.
// 'to' can be Pid, registered local name
// or a tuple {RegisteredName, NodeName}
func (p *Process) Cast(to interface{}, message etf.Term) {
	msg := etf.Term(etf.Tuple{etf.Atom("$gen_cast"), message})
	p.Node.registrar.route(p.self, to, msg)
}

// MonitorProcess creates monitor between the processes. When a process monitor
// is triggered, a 'DOWN' message is sent that has the following
// pattern: {'DOWN', MonitorRef, Type, Object, Info}
func (p *Process) MonitorProcess(to etf.Pid) etf.Ref {
	return p.Node.monitor.MonitorProcess(p.self, to)
}

// Link creates a link between the calling process and another process
func (p *Process) Link(with etf.Pid) {
	p.Node.monitor.Link(p.self, with)
}

// Unlink removes the link, if there is one, between the calling process and the process referred to by Pid.
func (p *Process) Unlink(with etf.Pid) {
	p.Node.monitor.Unink(p.self, with)
}

// MonitorNode creates monitor between the current process and node. If Node fails or does not exist,
// the message {nodedown, Node} is delivered to the process.
func (p *Process) MonitorNode(name string) etf.Ref {
	return p.Node.monitor.MonitorNode(p.self, name)
}

// DemonitorProcess removes monitor
func (p *Process) DemonitorProcess(ref etf.Ref) {
	p.Node.monitor.DemonitorProcess(ref)
}

// DemonitorNode removes monitor
func (p *Process) DemonitorNode(ref etf.Ref) {
	p.Node.monitor.DemonitorNode(ref)
}

// ListEnv returns map of configured environment variables.
// Process' environment is also inherited from environment variables
// of groupLeader (if its started as a child of Application/Supervisor)
func (p *Process) ListEnv() map[string]interface{} {
	var env map[string]interface{}
	if p.groupLeader == nil {
		env = make(map[string]interface{})
	} else {
		env = p.groupLeader.ListEnv()
	}

	p.RLock()
	defer p.RUnlock()
	for key, value := range p.env {
		env[key] = value
	}
	return env
}

// SetEnv set environment variable with given name
func (p *Process) SetEnv(name string, value interface{}) {
	p.Lock()
	defer p.Unlock()
	if p.env == nil {
		p.env = make(map[string]interface{})
	}
	p.env[name] = value
}

// GenEnv returns value associated with given environment name.
func (p *Process) GenEnv(name string) interface{} {
	p.RLock()
	defer p.RUnlock()

	if value, ok := p.env[name]; ok {
		return value
	}

	if p.groupLeader != nil {
		return p.groupLeader.GenEnv(name)
	}

	return nil
}
