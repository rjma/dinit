// Dinit is a mini init replacement useful for use inside Docker containers.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

var (
	timeout     time.Duration
	maxproc     float64
	start, stop string

	test bool // only used then testing

	cmds = NewCommands() // NewProcs
	prim = NewPrimary()
)

const testPid = 123

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: dinit [OPTION...] -r CMD [OPTION..] [-r CMD [OPTION...]]...")
		fmt.Fprintln(os.Stderr, "Start CMDs by passing the environment.")
		fmt.Fprintln(os.Stderr, "Distribute SIGHUP, SIGTERM and SIGINT to the processes.\n")
		flag.PrintDefaults()
	}

	// -r CMD [OPTION...]
	var cmd *exec.Cmd = nil
	commands := []*exec.Cmd{}
	seen := false

	for i := 0; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-r":
			if cmd != nil {
				commands = append(commands, cmd)
			}

			seen = true

			cmd = new(exec.Cmd)
			if i+1 == len(os.Args) {
				logFatalf("need a command after -r")
			}
			cmd.Args = append(cmd.Args, os.Args[i+1])
			cmd.Path = os.Args[i+1]

			// Clear the args to flag parsing keeps working.
			os.Args[i] = ""
			os.Args[i+1] = ""

			i++
			continue
		case "\\-r":
			os.Args[i] = "-r"
		}

		if seen {
			cmd.Args = append(cmd.Args, os.Args[i])
			os.Args[i] = ""
		}
	}
	if cmd != nil {
		commands = append(commands, cmd)
	}

	if len(commands) == 0 {
		flag.Usage()
		return
	}

	flag.DurationVar(&timeout, "timeout", envDuration("DINIT_TIMEOUT", 10*time.Second), "time in seconds between SIGTERM and SIGKILL (DINIT_TIMEOUT)")
	flag.Float64Var(&maxproc, "maxproc", 0.0, "set GOMAXPROCS to runtime.NumCPU() * maxproc, when GOMAXPROCS already set use that")
	flag.Float64Var(&maxproc, "core-fraction", 0.0, "set GOMAXPROCS to runtime.NumCPU() * core-fraction, when GOMAXPROCS already set use that")
	flag.StringVar(&start, "start", "", "command to run during startup, non-zero exit status abort dinit")
	flag.StringVar(&stop, "stop", "", "command to run during teardown")
	flag.Parse()

	if maxproc > 0.0 {
		if v := os.Getenv("GOMAXPROCS"); v != "" {
			logPrintf("GOMAXPROCS already set, using that value: %s", v)
		} else {
			numcpu := strconv.Itoa(int(math.Ceil(float64(runtime.NumCPU()) * maxproc)))
			logPrintf("using %s as GOMAXPROCS", numcpu)
			os.Setenv("GOMAXPROCS", numcpu)
		}
	}

	if start != "" {
		startcmd := command(start)
		if err := startcmd.Run(); err != nil {
			logFatalf("start command failed: %s", err)
		}
	}
	if stop != "" {
		stopcmd := command(stop)
		defer stopcmd.Run()
	}

	run(commands)
	wait()
}

// run runs the commands as given on the command line.
func run(commands []*exec.Cmd) {
	for i, _ := range commands {
		// Need to copy here, because otherwise the closure below will access
		// to wrong command, when we run in a loop.
		c := commands[i]
		if err := c.Start(); err != nil {
			logPrintf("%s", err)
			cmds.Cleanup(syscall.SIGINT)
			return
		}
		// TODO(miek): primary is going to be last
		if i == len(commands) - 1 {
			prim.Set(c.Process.Pid)
		}

		pid := c.Process.Pid
		if test {
			pid = testPid
		}
		logPrintf("pid %d started: %v", pid, c.Args)

		cmds.Insert(c)

		go func() {
			err := c.Wait()
			pid := c.Process.Pid
			if test {
				pid = testPid
			}
			logPrintf("pid %d, finished: %v with error: %v", pid, c.Args, err)
			cmds.Remove(c)
			if prim.Primary(c.Process.Pid) && cmds.Len() > 0 {
				logPrintf("pid %d was primary, signalling other processes", pid)
				cmds.Cleanup(syscall.SIGINT)
			}
		}()
	}
	return
}

// wait waits for commands to finish.
func wait() {
	defer func() { logPrintf("all processes exited, goodbye!") }()

	ints := make(chan os.Signal)
	signal.Notify(ints, syscall.SIGINT, syscall.SIGTERM)

	other := make(chan os.Signal)
	signal.Notify(other, syscall.SIGHUP)

	tick := time.Tick(100 * time.Millisecond) // 0.1 sec

	for {
		select {
		case <-tick:
			if cmds.Len() == 0 {
				return
			}
		case sig := <-other:
			cmds.Signal(sig)
		case sig := <-ints:
			cmds.Cleanup(sig)
		}
	}
}

func logPrintf(format string, v ...interface{}) {
	if test {
		fmt.Printf("dinit: "+format+"\n", v...)
		return
	}
	log.Printf("dinit: "+format, v...)
}

func logFatalf(format string, v ...interface{}) {
	log.Fatalf("dinit: "+format, v...)
}
