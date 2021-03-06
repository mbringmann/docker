package docker

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/client"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/pkg/common"
	"github.com/docker/docker/pkg/term"
	"github.com/kr/pty"
)

func closeWrap(args ...io.Closer) error {
	e := false
	ret := fmt.Errorf("Error closing elements")
	for _, c := range args {
		if err := c.Close(); err != nil {
			e = true
			ret = fmt.Errorf("%s\n%s", ret, err)
		}
	}
	if e {
		return ret
	}
	return nil
}

func setRaw(t *testing.T, c *daemon.Container) *term.State {
	pty, err := c.GetPtyMaster()
	if err != nil {
		t.Fatal(err)
	}
	state, err := term.MakeRaw(pty.Fd())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func unsetRaw(t *testing.T, c *daemon.Container, state *term.State) {
	pty, err := c.GetPtyMaster()
	if err != nil {
		t.Fatal(err)
	}
	term.RestoreTerminal(pty.Fd(), state)
}

func waitContainerStart(t *testing.T, timeout time.Duration) *daemon.Container {
	var container *daemon.Container

	setTimeout(t, "Waiting for the container to be started timed out", timeout, func() {
		for {
			l := globalDaemon.List()
			if len(l) == 1 && l[0].IsRunning() {
				container = l[0]
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	if container == nil {
		t.Fatal("An error occured while waiting for the container to start")
	}

	return container
}

func setTimeout(t *testing.T, msg string, d time.Duration, f func()) {
	c := make(chan bool)

	// Make sure we are not too long
	go func() {
		time.Sleep(d)
		c <- true
	}()
	go func() {
		f()
		c <- false
	}()
	if <-c && msg != "" {
		t.Fatal(msg)
	}
}

func expectPipe(expected string, r io.Reader) error {
	o, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.Trim(o, " \r\n") != expected {
		return fmt.Errorf("Unexpected output. Expected [%s], received [%s]", expected, o)
	}
	return nil
}

func assertPipe(input, output string, r io.Reader, w io.Writer, count int) error {
	for i := 0; i < count; i++ {
		if _, err := w.Write([]byte(input)); err != nil {
			return err
		}
		if err := expectPipe(output, r); err != nil {
			return err
		}
	}
	return nil
}

// TestRunDetach checks attaching and detaching with the escape sequence.
func TestRunDetach(t *testing.T) {
	stdout, stdoutPipe := io.Pipe()
	cpty, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	cli := client.NewDockerCli(tty, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)
	defer cleanup(globalEngine, t)

	ch := make(chan struct{})
	go func() {
		defer close(ch)
		cli.CmdRun("-i", "-t", unitTestImageID, "cat")
	}()

	container := waitContainerStart(t, 10*time.Second)

	state := setRaw(t, container)
	defer unsetRaw(t, container, state)

	setTimeout(t, "First read/write assertion timed out", 2*time.Second, func() {
		if err := assertPipe("hello\n", "hello", stdout, cpty, 150); err != nil {
			t.Fatal(err)
		}
	})

	setTimeout(t, "Escape sequence timeout", 5*time.Second, func() {
		cpty.Write([]byte{16})
		time.Sleep(100 * time.Millisecond)
		cpty.Write([]byte{17})
	})

	// wait for CmdRun to return
	setTimeout(t, "Waiting for CmdRun timed out", 15*time.Second, func() {
		<-ch
	})
	closeWrap(cpty, stdout, stdoutPipe)

	time.Sleep(500 * time.Millisecond)
	if !container.IsRunning() {
		t.Fatal("The detached container should be still running")
	}

	setTimeout(t, "Waiting for container to die timed out", 20*time.Second, func() {
		container.Kill()
	})
}

// TestAttachDetach checks that attach in tty mode can be detached using the long container ID
func TestAttachDetach(t *testing.T) {
	stdout, stdoutPipe := io.Pipe()
	cpty, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	cli := client.NewDockerCli(tty, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)
	defer cleanup(globalEngine, t)

	ch := make(chan struct{})
	go func() {
		defer close(ch)
		if err := cli.CmdRun("-i", "-t", "-d", unitTestImageID, "cat"); err != nil {
			t.Fatal(err)
		}
	}()

	container := waitContainerStart(t, 10*time.Second)

	setTimeout(t, "Reading container's id timed out", 10*time.Second, func() {
		buf := make([]byte, 1024)
		n, err := stdout.Read(buf)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Trim(string(buf[:n]), " \r\n") != container.ID {
			t.Fatalf("Wrong ID received. Expect %s, received %s", container.ID, buf[:n])
		}
	})
	setTimeout(t, "Starting container timed out", 10*time.Second, func() {
		<-ch
	})

	state := setRaw(t, container)
	defer unsetRaw(t, container, state)

	stdout, stdoutPipe = io.Pipe()
	cpty, tty, err = pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	cli = client.NewDockerCli(tty, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)

	ch = make(chan struct{})
	go func() {
		defer close(ch)
		if err := cli.CmdAttach(container.ID); err != nil {
			if err != io.ErrClosedPipe {
				t.Fatal(err)
			}
		}
	}()

	setTimeout(t, "First read/write assertion timed out", 2*time.Second, func() {
		if err := assertPipe("hello\n", "hello", stdout, cpty, 150); err != nil {
			if err != io.ErrClosedPipe {
				t.Fatal(err)
			}
		}
	})

	setTimeout(t, "Escape sequence timeout", 5*time.Second, func() {
		cpty.Write([]byte{16})
		time.Sleep(100 * time.Millisecond)
		cpty.Write([]byte{17})
	})

	// wait for CmdRun to return
	setTimeout(t, "Waiting for CmdAttach timed out", 15*time.Second, func() {
		<-ch
	})

	closeWrap(cpty, stdout, stdoutPipe)

	time.Sleep(500 * time.Millisecond)
	if !container.IsRunning() {
		t.Fatal("The detached container should be still running")
	}

	setTimeout(t, "Waiting for container to die timedout", 5*time.Second, func() {
		container.Kill()
	})
}

// TestAttachDetachTruncatedID checks that attach in tty mode can be detached
func TestAttachDetachTruncatedID(t *testing.T) {
	stdout, stdoutPipe := io.Pipe()
	cpty, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	cli := client.NewDockerCli(tty, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)
	defer cleanup(globalEngine, t)

	// Discard the CmdRun output
	go stdout.Read(make([]byte, 1024))
	setTimeout(t, "Starting container timed out", 2*time.Second, func() {
		if err := cli.CmdRun("-i", "-t", "-d", unitTestImageID, "cat"); err != nil {
			t.Fatal(err)
		}
	})

	container := waitContainerStart(t, 10*time.Second)

	state := setRaw(t, container)
	defer unsetRaw(t, container, state)

	stdout, stdoutPipe = io.Pipe()
	cpty, tty, err = pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	cli = client.NewDockerCli(tty, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)

	ch := make(chan struct{})
	go func() {
		defer close(ch)
		if err := cli.CmdAttach(common.TruncateID(container.ID)); err != nil {
			if err != io.ErrClosedPipe {
				t.Fatal(err)
			}
		}
	}()

	setTimeout(t, "First read/write assertion timed out", 2*time.Second, func() {
		if err := assertPipe("hello\n", "hello", stdout, cpty, 150); err != nil {
			if err != io.ErrClosedPipe {
				t.Fatal(err)
			}
		}
	})

	setTimeout(t, "Escape sequence timeout", 5*time.Second, func() {
		cpty.Write([]byte{16})
		time.Sleep(100 * time.Millisecond)
		cpty.Write([]byte{17})
	})

	// wait for CmdRun to return
	setTimeout(t, "Waiting for CmdAttach timed out", 15*time.Second, func() {
		<-ch
	})
	closeWrap(cpty, stdout, stdoutPipe)

	time.Sleep(500 * time.Millisecond)
	if !container.IsRunning() {
		t.Fatal("The detached container should be still running")
	}

	setTimeout(t, "Waiting for container to die timedout", 5*time.Second, func() {
		container.Kill()
	})
}

// Expected behaviour, the process stays alive when the client disconnects
func TestAttachDisconnect(t *testing.T) {
	stdout, stdoutPipe := io.Pipe()
	cpty, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	cli := client.NewDockerCli(tty, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)
	defer cleanup(globalEngine, t)

	go func() {
		// Start a process in daemon mode
		if err := cli.CmdRun("-d", "-i", unitTestImageID, "/bin/cat"); err != nil {
			log.Debugf("Error CmdRun: %s", err)
		}
	}()

	setTimeout(t, "Waiting for CmdRun timed out", 10*time.Second, func() {
		if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	})

	setTimeout(t, "Waiting for the container to be started timed out", 10*time.Second, func() {
		for {
			l := globalDaemon.List()
			if len(l) == 1 && l[0].IsRunning() {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	container := globalDaemon.List()[0]

	// Attach to it
	c1 := make(chan struct{})
	go func() {
		// We're simulating a disconnect so the return value doesn't matter. What matters is the
		// fact that CmdAttach returns.
		cli.CmdAttach(container.ID)
		close(c1)
	}()

	setTimeout(t, "First read/write assertion timed out", 2*time.Second, func() {
		if err := assertPipe("hello\n", "hello", stdout, cpty, 150); err != nil {
			t.Fatal(err)
		}
	})
	// Close pipes (client disconnects)
	if err := closeWrap(cpty, stdout, stdoutPipe); err != nil {
		t.Fatal(err)
	}

	// Wait for attach to finish, the client disconnected, therefore, Attach finished his job
	setTimeout(t, "Waiting for CmdAttach timed out", 2*time.Second, func() {
		<-c1
	})

	// We closed stdin, expect /bin/cat to still be running
	// Wait a little bit to make sure container.monitor() did his thing
	_, err = container.WaitStop(500 * time.Millisecond)
	if err == nil || !container.IsRunning() {
		t.Fatalf("/bin/cat is not running after closing stdin")
	}

	// Try to avoid the timeout in destroy. Best effort, don't check error
	cStdin := container.StdinPipe()
	cStdin.Close()
	container.WaitStop(-1 * time.Second)
}

// Expected behaviour: container gets deleted automatically after exit
func TestRunAutoRemove(t *testing.T) {
	t.Skip("Fixme. Skipping test for now, race condition")
	stdout, stdoutPipe := io.Pipe()

	cli := client.NewDockerCli(nil, stdoutPipe, ioutil.Discard, "", testDaemonProto, testDaemonAddr, nil)
	defer cleanup(globalEngine, t)

	c := make(chan struct{})
	go func() {
		defer close(c)
		if err := cli.CmdRun("--rm", unitTestImageID, "hostname"); err != nil {
			t.Fatal(err)
		}
	}()

	var temporaryContainerID string
	setTimeout(t, "Reading command output time out", 2*time.Second, func() {
		cmdOutput, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		temporaryContainerID = cmdOutput
		if err := closeWrap(stdout, stdoutPipe); err != nil {
			t.Fatal(err)
		}
	})

	setTimeout(t, "CmdRun timed out", 10*time.Second, func() {
		<-c
	})

	time.Sleep(500 * time.Millisecond)

	if len(globalDaemon.List()) > 0 {
		t.Fatalf("failed to remove container automatically: container %s still exists", temporaryContainerID)
	}
}
