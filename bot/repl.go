package bot

import (
	"context"
	"fmt"
	"github.com/creack/pty"
	"golang.org/x/sync/errgroup"
	"heckel.io/replbot/util"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type repl struct {
	ctx           context.Context
	sessionID     string
	sender        Sender
	userInputChan chan string
	runCmd        *exec.Cmd
	killCmd       *exec.Cmd
	ptmx          *os.File
	outChan       chan []byte
	exitMarker    string
}

func runREPL(ctx context.Context, sessionID string, sender Sender, userInputChan chan string, script string) error {
	r := newREPL(ctx, sessionID, sender, userInputChan, script)
	return r.Exec()
}

func newREPL(ctx context.Context, sessionID string, sender Sender, userInputChan chan string, script string) *repl {
	scriptID := util.RandomStringWithCharset(10, charsetRandomID)
	exitMarker := util.RandomStringWithCharset(10, charsetRandomID)
	return &repl{
		ctx:           ctx,
		sessionID:     sessionID,
		sender:        sender,
		userInputChan: userInputChan,
		runCmd:        exec.Command("sh", "-c", fmt.Sprintf(runScript, script, scriptID, exitMarker)),
		killCmd:       exec.Command("sh", "-c", fmt.Sprintf(killScript, script, scriptID)),
		outChan:       make(chan []byte, 10),
		exitMarker:    exitMarker,
	}
}

func (r *repl) Exec() error {
	log.Printf("[session %s] Started REPL session", r.sessionID)
	defer log.Printf("[session %s] Closed REPL session", r.sessionID)

	var err error
	if err = r.sender.Send(sessionStartedMessage, Text); err != nil {
		return err
	}
	r.ptmx, err = pty.Start(r.runCmd)
	if err != nil {
		return fmt.Errorf("cannot start REPL session: %s", err.Error())
	}
	var g *errgroup.Group
	g, r.ctx = errgroup.WithContext(r.ctx)
	g.Go(r.userInputLoop)
	g.Go(r.commandOutputLoop)
	g.Go(r.commandOutputForwarder)
	g.Go(r.cleanupListener)
	return g.Wait()
}

func (r *repl) userInputLoop() error {
	log.Printf("[session %s] Started user input loop", r.sessionID)
	defer log.Printf("[session %s] Exiting user input loop", r.sessionID)
	for {
		select {
		case line := <-r.userInputChan:
			if err := r.handleUserInput(line, r.ptmx); err != nil {
				return err
			}
		case <-r.ctx.Done():
			return errExit
		}
	}
}
func (r *repl) handleUserInput(input string, outputWriter io.Writer) error {
	switch input {
	case helpCommand:
		return r.sender.Send(availableCommandsMessage, Markdown)
	case exitCommand:
		return errExit
	default:
		// TODO properly handle empty lines
		if strings.HasPrefix(input, commentPrefix) {
			return nil // Ignore comments
		} else if controlChar, ok := controlCharTable[input[1:]]; ok {
			_, err := outputWriter.Write([]byte{controlChar})
			return err
		}
		_, err := io.WriteString(outputWriter, fmt.Sprintf("%s\n", input))
		return err
	}
}

func (r *repl) commandOutputLoop() error {
	log.Printf("[session %s] Started command output loop", r.sessionID)
	defer log.Printf("[session %s] Exiting command output loop", r.sessionID)
	for {
		buf := make([]byte, 512) // Allocation in a loop, ahhh ...
		n, err := r.ptmx.Read(buf)
		select {
		case <-r.ctx.Done():
			return errExit
		default:
			if e, ok := err.(*os.PathError); ok && e.Err == syscall.EIO {
				// An expected error when the ptmx is closed to break the Read() call.
				// Since we don't want to send this error to the user, we convert it to errExit.
				return errExit
			} else if err == io.EOF {
				if n > 0 {
					select {
					case r.outChan <- buf[:n]:
					case <-r.ctx.Done():
						return errExit
					}
				}
				return errExit
			} else if err != nil {
				return err
			} else if strings.TrimSpace(string(buf[:n])) == r.exitMarker {
				return errExit
			} else if n > 0 {
				select {
				case r.outChan <- buf[:n]:
				case <-r.ctx.Done():
					return errExit
				}
			}
		}
	}
}

func (r *repl) commandOutputForwarder() error {
	log.Printf("[session %s] Started command output forwarder", r.sessionID)
	defer log.Printf("[session %s] Exiting command output forwarder", r.sessionID)
	var message string
	for {
		select {
		case result := <-r.outChan:
			message += shellEscapeRegex.ReplaceAllString(string(result), "")
			if len(message) > maxMessageLength {
				if err := r.sender.Send(message, Code); err != nil {
					return err
				}
				message = ""
			}
		case <-time.After(300 * time.Millisecond):
			if len(message) > 0 {
				if err := r.sender.Send(message, Code); err != nil {
					return err
				}
				message = ""
			}
		case <-r.ctx.Done():
			if len(message) > 0 {
				if err := r.sender.Send(message, Code); err != nil {
					return err
				}
			}
			return errExit
		}
	}
}

func (r *repl) cleanupListener() error {
	log.Printf("[session %s] Started command cleanup listener", r.sessionID)
	defer log.Printf("[session %s] Command cleanupListener finished", r.sessionID)
	<-r.ctx.Done()
	if err := r.killCmd.Start(); err != nil {
		log.Printf("warning: %s", err.Error())
	}
	for i := 0; i < 20; i++ {
		if r.killCmd.ProcessState == nil || r.killCmd.ProcessState.Exited() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := util.KillChildProcesses(r.runCmd.Process.Pid); err != nil {
		log.Printf("[session %s] warning: %s", r.sessionID, err.Error())
	}
	if err := r.ptmx.Close(); err != nil {
		log.Printf("[session %s] warning: %s", r.sessionID, err.Error())
	}
	return nil
}