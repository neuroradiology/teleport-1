/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sshutils

import (
	"fmt"
	"io"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"

	"golang.org/x/crypto/ssh"
)

// NewUpstream returns new upstream connection to the server
func NewUpstream(clt *ssh.Client) (*Upstream, error) {
	session, err := clt.NewSession()
	if err != nil {
		clt.Close()
		return nil, trace.Wrap(err)
	}
	return &Upstream{
		addr:    clt.Conn.RemoteAddr().String(),
		client:  clt,
		session: session,
	}, nil
}

// DialUpstream dials remote server and returns upstream
func DialUpstream(username, addr string, signers []ssh.Signer) (*Upstream, error) {
	sshConfig := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)},
	}
	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	return &Upstream{
		addr:    addr,
		client:  client,
		session: session,
	}, nil
}

// Upstream is a wrapper around SSH client connection
// that provides some handy functions to work with interactive shells
// and launching commands
type Upstream struct {
	sync.Mutex

	addr    string
	client  *ssh.Client
	session *ssh.Session

	prefix []byte
}

func (u *Upstream) SetPrefix(data []byte) {
	u.Lock()
	defer u.Unlock()

	u.prefix = data
}

// GetClient returns current active ssh client
func (u *Upstream) GetClient() *ssh.Client {
	return u.client
}

// GetSession returns current active sesson
func (u *Upstream) GetSession() *ssh.Session {
	return u.session
}

// Close closes session and client connection
func (u *Upstream) Close() error {
	return CloseAll(u.session, u.client)
}

// String returns debug-friendly information about this upstream
func (u *Upstream) String() string {
	return fmt.Sprintf("upstream(addr=%v)", u.addr)
}

// Wait waits for the session to complete
func (u *Upstream) Wait() error {
	return u.session.Wait()
}

// CommandRW executes a command and returns read writer to communicate
// with the process using it's stdin and stdout
func (u *Upstream) CommandRW(command string) (io.ReadWriter, error) {
	stdout, err := u.session.StdoutPipe()
	if err != nil {
		return nil, trace.Wrap(err, "failed to pipe stdout")
	}
	stdin, err := u.session.StdinPipe()
	if err != nil {
		return nil, trace.Wrap(err, "fail to pipe stdin")
	}
	err = u.session.Start(command)
	if err != nil {
		return nil, trace.Wrap(err,
			fmt.Sprintf("pipe failed to start command '%v'", command))
	}
	return &combo{r: stdout, w: stdin}, nil
}

// PipeCommand pipes input and output to the read writer, returns
// result code of the command execution
func (u *Upstream) PipeCommand(ch io.ReadWriter, command string) (int, error) {
	stderr, err := u.session.StderrPipe()
	if err != nil {
		return -1, trace.Wrap(err, "fail to pipe stderr")
	}
	stdout, err := u.session.StdoutPipe()
	if err != nil {
		return -1, trace.Wrap(err, "fail to pipe stdout")
	}
	stdin, err := u.session.StdinPipe()
	if err != nil {
		return -1, trace.Wrap(err, "fail to pipe stdin")
	}
	closeC := make(chan error, 4)

	err = u.session.Start(command)
	if err != nil {
		return -1, trace.Wrap(err,
			fmt.Sprintf("pipe failed to start command '%v'", command))
	}

	go func() {
		_, err := io.Copy(stdin, ch)
		closeC <- err
	}()

	go func() {
		_, err := io.Copy(ch, stderr)
		closeC <- err
	}()

	go func() {
		_, err := io.Copy(ch, stdout)
		closeC <- err
	}()

	go func() {
		closeC <- u.session.Wait()
	}()

	err = <-closeC
	if err != nil {
		if err, ok := err.(*ssh.ExitError); ok {
			return err.ExitStatus(), nil
		}
		return -1, trace.Wrap(err,
			fmt.Sprintf("failed to collect status of a command '%v'", command))
	}
	return 0, nil
}

// PipeShell starts interactive shell and pipes stdin, stdout and stderr
// to the given read writer
func (u *Upstream) PipeShell(rw io.ReadWriter, req *PTYReqParams) error {
	targetStderr, err := u.session.StderrPipe()
	if err != nil {
		return trace.Wrap(err, "fail to pipe stderr")
	}
	targetStdout, err := u.session.StdoutPipe()
	if err != nil {
		return trace.Wrap(err, "fail to pipe stdout")
	}
	targetStdin, err := u.session.StdinPipe()
	if err != nil {
		return trace.Wrap(err, "fail to pipe stdin")
	}
	closeC := make(chan error, 4)

	if err := u.session.Shell(); err != nil {
		return trace.Wrap(err, "failed to start shell")
	}

	if req != nil {
		u.session.SendRequest(PTYReq, false, ssh.Marshal(*req))
	}

	// getPrefix protects u.prefix with a mutex
	getPrefix := func() []byte {
		u.Lock()
		defer u.Unlock()
		return u.prefix
	}

	// copyOutput works exactly like io.Copy() but it does two additional things:
	// It appends 'prefix' to the end of every write (used to send screensize back to
	// the web client in real time (it MUST know the screen size ahead of every write)
	copyOutput := func(w io.Writer, r io.Reader) (err error) {
		written, n := 0, 0
		const buflen = 16 * 1024
		buffer := make([]byte, buflen)
		for err == nil {
			prefix := getPrefix()
			n, err = r.Read(buffer)
			if err != nil {
				break
			}
			if prefix != nil {
				pl := len(prefix)
				if pl+n <= buflen {
					copy(buffer[n:], prefix)
					n += pl
				}
			}
			written, err = w.Write(buffer[:n])
			if written != n {
				err = io.ErrShortWrite
			}
		}
		if err != io.EOF {
			logrus.Error(err)
		}
		return err
	}

	go func() {
		_, err := io.Copy(targetStdin, rw)
		closeC <- err
	}()

	go func() {
		closeC <- copyOutput(rw, targetStderr)
	}()

	go func() {
		closeC <- copyOutput(rw, targetStdout)
	}()

	go func() {
		closeC <- u.session.Wait()
	}()

	return <-closeC
}

type combo struct {
	r io.Reader
	w io.Writer
}

func (c *combo) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

func (c *combo) Write(b []byte) (int, error) {
	return c.w.Write(b)
}
