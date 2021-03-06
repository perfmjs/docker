package client

import (
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/engine"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/signal"
)

func (cli *DockerCli) forwardAllSignals(cid string) chan os.Signal {
	sigc := make(chan os.Signal, 128)
	signal.CatchAll(sigc)
	go func() {
		for s := range sigc {
			if s == signal.SIGCHLD {
				continue
			}
			var sig string
			for sigStr, sigN := range signal.SignalMap {
				if sigN == s {
					sig = sigStr
					break
				}
			}
			if sig == "" {
				logrus.Errorf("Unsupported signal: %v. Discarding.", s)
			}
			if _, _, err := readBody(cli.call("POST", fmt.Sprintf("/containers/%s/kill?signal=%s", cid, sig), nil, nil)); err != nil {
				logrus.Debugf("Error sending signal: %s", err)
			}
		}
	}()
	return sigc
}

// CmdStart starts one or more stopped containers.
//
// Usage: docker start [OPTIONS] CONTAINER [CONTAINER...]
func (cli *DockerCli) CmdStart(args ...string) error {
	var (
		cErr chan error
		tty  bool

		cmd       = cli.Subcmd("start", "CONTAINER [CONTAINER...]", "Start one or more stopped containers", true)
		attach    = cmd.Bool([]string{"a", "-attach"}, false, "Attach STDOUT/STDERR and forward signals")
		openStdin = cmd.Bool([]string{"i", "-interactive"}, false, "Attach container's STDIN")
	)

	cmd.Require(flag.Min, 1)
	cmd.ParseFlags(args, true)

	if *attach || *openStdin {
		if cmd.NArg() > 1 {
			return fmt.Errorf("You cannot start and attach multiple containers at once.")
		}

		stream, _, err := cli.call("GET", "/containers/"+cmd.Arg(0)+"/json", nil, nil)
		if err != nil {
			return err
		}

		env := engine.Env{}
		if err := env.Decode(stream); err != nil {
			return err
		}
		config := env.GetSubEnv("Config")
		tty = config.GetBool("Tty")

		if !tty {
			sigc := cli.forwardAllSignals(cmd.Arg(0))
			defer signal.StopCatch(sigc)
		}

		var in io.ReadCloser

		v := url.Values{}
		v.Set("stream", "1")

		if *openStdin && config.GetBool("OpenStdin") {
			v.Set("stdin", "1")
			in = cli.in
		}

		v.Set("stdout", "1")
		v.Set("stderr", "1")

		hijacked := make(chan io.Closer)
		// Block the return until the chan gets closed
		defer func() {
			logrus.Debugf("CmdStart() returned, defer waiting for hijack to finish.")
			if _, ok := <-hijacked; ok {
				logrus.Errorf("Hijack did not finish (chan still open)")
			}
			cli.in.Close()
		}()
		cErr = promise.Go(func() error {
			return cli.hijack("POST", "/containers/"+cmd.Arg(0)+"/attach?"+v.Encode(), tty, in, cli.out, cli.err, hijacked, nil)
		})

		// Acknowledge the hijack before starting
		select {
		case closer := <-hijacked:
			// Make sure that the hijack gets closed when returning (results
			// in closing the hijack chan and freeing server's goroutines)
			if closer != nil {
				defer closer.Close()
			}
		case err := <-cErr:
			if err != nil {
				return err
			}
		}
	}

	var encounteredError error
	for _, name := range cmd.Args() {
		_, _, err := readBody(cli.call("POST", "/containers/"+name+"/start", nil, nil))
		if err != nil {
			if !*attach && !*openStdin {
				// attach and openStdin is false means it could be starting multiple containers
				// when a container start failed, show the error message and start next
				fmt.Fprintf(cli.err, "%s\n", err)
				encounteredError = fmt.Errorf("Error: failed to start one or more containers")
			} else {
				encounteredError = err
			}
		} else {
			if !*attach && !*openStdin {
				fmt.Fprintf(cli.out, "%s\n", name)
			}
		}
	}

	if encounteredError != nil {
		return encounteredError
	}

	if *openStdin || *attach {
		if tty && cli.isTerminalOut {
			if err := cli.monitorTtySize(cmd.Arg(0), false); err != nil {
				logrus.Errorf("Error monitoring TTY size: %s", err)
			}
		}
		if attchErr := <-cErr; attchErr != nil {
			return attchErr
		}
		_, status, err := getExitCode(cli, cmd.Arg(0))
		if err != nil {
			return err
		}
		if status != 0 {
			return StatusError{StatusCode: status}
		}
	}
	return nil
}
