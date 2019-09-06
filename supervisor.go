package ergonode

import (
	"fmt"

	"github.com/halturin/ergonode/etf"
	"github.com/halturin/ergonode/lib"
)

type SupervisorStrategy struct {
	Type      SupervisorStrategyType
	Intensity uint16
	Period    uint16
}

type SupervisorStrategyType = string
type SupervisorChildRestart = string
type SupervisorChild = string

const (
	// Restart strategies:

	// SupervisorRestartIntensity
	SupervisorRestartIntensity = uint16(10)

	// SupervisorRestartPeriod
	SupervisorRestartPeriod = uint16(10)

	// SupervisorStrategyOneForOne If one child process terminates and is to be restarted, only
	// that child process is affected. This is the default restart strategy.
	SupervisorStrategyOneForOne = "one_for_one"

	// SupervisorStrategyOneForAll If one child process terminates and is to be restarted, all other
	// child processes are terminated and then all child processes are restarted.
	SupervisorStrategyOneForAll = "one_for_all"

	// SupervisorStrategyRestForOne If one child process terminates and is to be restarted,
	// the 'rest' of the child processes (that is, the child
	// processes after the terminated child process in the start order)
	// are terminated. Then the terminated child process and all
	// child processes after it are restarted
	SupervisorStrategyRestForOne = "rest_for_one"

	// SupervisorStrategySimpleOneForOne A simplified one_for_one supervisor, where all
	// child processes are dynamically added instances
	// of the same process type, that is, running the same code.
	SupervisorStrategySimpleOneForOne = "simple_one_for_one"

	// Restart types:

	// SupervisorChildRestartPermanent child process is always restarted
	SupervisorChildRestartPermanent = "permanent"

	// SupervisorChildRestartTemporary child process is never restarted
	// (not even when the supervisor restart strategy is rest_for_one
	// or one_for_all and a sibling death causes the temporary process
	// to be terminated)
	SupervisorChildRestartTemporary = "temporary"

	// SupervisorChildRestartTransient child process is restarted only if
	// it terminates abnormally, that is, with an exit reason other
	// than normal, shutdown, or {shutdown,Term}.
	SupervisorChildRestartTransient = "transient"
)

// SupervisorBehavior interface
type SupervisorBehavior interface {
	Init(args ...interface{}) SupervisorSpec
}

type SupervisorSpec struct {
	children []SupervisorChildSpec
	strategy SupervisorStrategy
}

type SupervisorChildSpec struct {
	name     string
	child    interface{}
	args     []interface{}
	restart  SupervisorChildRestart
	disabled bool
}

// Supervisor is implementation of ProcessBehavior interface
type Supervisor struct{}

func (sv *Supervisor) loop(p *Process, object interface{}, args ...interface{}) string {
	spec := object.(SupervisorBehavior).Init(args...)
	lib.Log("Supervisor spec %#v\n", spec)
	p.ready <- true

	p.children = make([]*Process, len(spec.children))
	sv.initChildren(p, spec.children)

	fmt.Println("CHILDREN", p.children)
	stop := make(chan string, 2)

	p.currentFunction = "Supervisor:loop"

	waitTerminatingProcesses := []etf.Pid{}

	for {
		var message etf.Term
		var fromPid etf.Pid

		select {
		case reason := <-stop:
			return reason

		case msg := <-p.mailBox:
			fromPid = msg.Element(1).(etf.Pid)
			message = msg.Element(2)

		case <-p.Context.Done():
			return "shutdown"
		}

		p.reductions++

		lib.Log("[%#v]. Message from %#v\n", p.self, fromPid)

		switch m := message.(type) {

		case etf.Tuple:

			switch m.Element(1) {

			case etf.Atom("EXIT"):

				terminated := m.Element(2).(etf.Pid)
				reason := m.Element(3).(etf.Atom)

				fmt.Println("CHILD TERMINATED:", terminated, "with reason:", reason)

				if len(waitTerminatingProcesses) > 0 {

					for i := range waitTerminatingProcesses {
						if waitTerminatingProcesses[i] == terminated {
							waitTerminatingProcesses[0] = waitTerminatingProcesses[i]
							waitTerminatingProcesses = waitTerminatingProcesses[1:]
						}
					}

					if len(waitTerminatingProcesses) == 0 {
						// it was the last one. lets restart all terminated children
						restart := etf.Tuple{etf.Pid{}, etf.Atom("$restart")}
						p.mailBox <- restart
					}

					continue
				}

				switch spec.strategy.Type {

				case SupervisorStrategyOneForAll:
					for i := range p.children {
						if p.children[i].self == terminated {
							disable := haveToDisableChild(spec.children[i].restart, reason)
							spec.children[i].disabled = disable
							continue
						}
						p.children[i].Stop("shutdown")
						waitTerminatingProcesses = append(waitTerminatingProcesses, p.children[i].self)
					}

				case SupervisorStrategyRestForOne:
					isRest := false
					for i := range p.children {
						if p.children[i].self == terminated {
							isRest = true
							disable := haveToDisableChild(spec.children[i].restart, reason)
							spec.children[i].disabled = disable
							continue
						}

						if isRest {
							p.children[i].Stop("shutdown")
							waitTerminatingProcesses = append(waitTerminatingProcesses, p.children[i].self)
						}
					}

				case SupervisorStrategyOneForOne:
					for i := range p.children {
						if p.children[i].self == terminated {
							// haveToDisableChild(spec.children[i].restart, reason)
							// spec.children[i].state = restart
							// restart := etf.Tuple{etf.Pid{}, etf.Atom("$restart")}
							// p.mailBox <- restart
							// continue
						}
					}
				case SupervisorStrategySimpleOneForOne:

				}

			default:
				lib.Log("m: %#v", m)
			}
		case etf.Atom:
			switch m {
			case etf.Atom("$restart"):
				sv.initChildren(p, spec.children)
			}
		default:
			lib.Log("m: %#v", m)
		}
	}
}

func haveToDisableChild(restart SupervisorChildRestart, reason etf.Atom) bool {
	switch restart {
	case SupervisorChildRestartTransient:
		if reason == etf.Atom("shutdown") || reason == etf.Atom("normal") {
			return true
		}

	case SupervisorChildRestartTemporary:
		return true

	}

	return false
}

func (sv *Supervisor) initChildren(parent *Process, specs []SupervisorChildSpec) {

	for i := range specs {

		if parent.children[i] != nil {
			// its already running
			continue
		}

		spec := specs[i]
		if spec.disabled {
			// its been disabled due to restart strategy
			continue
		}

		opts := ProcessOptions{}
		emptyPid := etf.Pid{}

		if parent.groupLeader == emptyPid {
			// leader is not set
			opts.GroupLeader = parent.self
		} else {
			opts.GroupLeader = parent.groupLeader
		}
		// if simple_one_for_one -> spec.name have to be omitted (process shouldn't have
		// to be registered with name)
		process := parent.Node.Spawn(spec.name, opts, spec.child, spec.args...)
		parent.Link(process.self)
		parent.children[i] = process
	}
}
