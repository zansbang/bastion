package sshd

import (
	"github.com/yankeguo/bastion/types"
	"golang.org/x/crypto/ssh"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"context"
	"net"
	"fmt"
	"log"
	"github.com/yankeguo/bastion/sshd/sandbox"
)

const (
	keyMode    = "bastion-mode"
	keyAccount = "bastion-account"
	keyUser    = "bastion-user"
	keyAddress = "bastion-address"

	modeLv1 = "lv1"
	modeLv2 = "lv2"

	channelTypeDirectTCPIP = "direct-tcpip"
	channelTypeSession     = "session"
)

type SSHD struct {
	opts            types.SSHDOptions
	clientSigners   []ssh.Signer
	hostSigner      ssh.Signer
	sshServerConfig *ssh.ServerConfig
	rpcConn         *grpc.ClientConn
	listener        net.Listener
	sandboxManager  sandbox.Manager
}

func New(opts types.SSHDOptions) *SSHD {
	return &SSHD{opts: opts}
}

func (s *SSHD) initSandboxManager() (err error) {
	s.sandboxManager, err = sandbox.NewManager(s.opts)
	return
}

func (s *SSHD) initRPCConn() (err error) {
	s.rpcConn, err = grpc.Dial(s.opts.DaemonEndpoint, grpc.WithInsecure())
	return
}

func (s *SSHD) initHostSigner() (err error) {
	s.hostSigner, err = loadSSHPrivateKeyFile(s.opts.HostKey)
	return
}

func (s *SSHD) initClientSigners() (err error) {
	s.clientSigners = []ssh.Signer{}
	for _, key := range s.opts.ClientKeys {
		var cs ssh.Signer
		if cs, err = loadSSHPrivateKeyFile(key); err != nil {
			return
		}
		s.clientSigners = append(s.clientSigners, cs)
	}
	return
}

func (s *SSHD) initSSHServerConfig() (err error) {
	s.sshServerConfig = &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (ms *ssh.Permissions, err error) {
			// create the services
			us, ks := types.NewUserServiceClient(s.rpcConn), types.NewKeyServiceClient(s.rpcConn)
			// decode target user and target node
			tu, th := decodeTargetServer(conn.User())
			// find the key
			var kRes *types.GetKeyResponse
			if kRes, err = ks.GetKey(context.Background(), &types.GetKeyRequest{Fingerprint: ssh.FingerprintSHA256(key)}); err != nil {
				return
			}
			// touch the key
			ks.TouchKey(context.Background(), &types.TouchKeyRequest{Fingerprint: kRes.Key.Fingerprint})
			// find the user
			var uRes *types.GetUserResponse
			if uRes, err = us.GetUser(context.Background(), &types.GetUserRequest{Account: kRes.Key.Account}); err != nil {
				return
			}
			// check blocked user
			if uRes.User.IsBlocked {
				err = errors.New("user is blocked")
				return
			}
			// touch the user
			us.TouchUser(context.Background(), &types.TouchUserRequest{Account: uRes.User.Account})
			// check internal connection
			if isSandboxConnection(conn, s.opts.SandboxEndpoint) {
				// connection from sandbox
				ns, rs := types.NewNodeServiceClient(s.rpcConn), types.NewGrantServiceClient(s.rpcConn)
				// check target user and target hostname and confirm it's a sandbox key
				if len(tu) == 0 || len(th) == 0 || kRes.Key.Source != types.KeySourceSandbox {
					err = errors.New("invalid target or invalid ssh key")
					return
				}
				var nRes *types.GetNodeResponse
				if nRes, err = ns.GetNode(context.Background(), &types.GetNodeRequest{Hostname: th}); err != nil {
					return
				}
				// check grant
				var cRes *types.CheckGrantResponse
				if cRes, err = rs.CheckGrant(context.Background(), &types.CheckGrantRequest{
					User:     tu,
					Account:  uRes.User.Account,
					Hostname: nRes.Node.Hostname,
				}); err != nil {
					return
				}
				if !cRes.Ok {
					err = errors.New("no permission")
					return
				}
				ms = &ssh.Permissions{
					Extensions: map[string]string{
						keyAccount: uRes.User.Account,
						keyUser:    tu,
						keyAddress: nRes.Node.Address,
						keyMode:    modeLv2,
					},
				}
			} else {
				// connection from external
				// check recursive sandbox connection
				if kRes.Key.Source == types.KeySourceSandbox {
					err = errors.New("recursive sandbox connection")
					return
				}
				ms = &ssh.Permissions{
					Extensions: map[string]string{
						keyAccount: uRes.User.Account,
						keyMode:    modeLv1,
					},
				}
			}
			return
		},
	}
	s.sshServerConfig.AddHostKey(s.hostSigner)
	return
}

func (s *SSHD) initListener() (err error) {
	s.listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", s.opts.Host, s.opts.Port))
	return
}

func (s *SSHD) Run() (err error) {
	// init host signer
	if err = s.initHostSigner(); err != nil {
		return
	}
	// init client signers
	if err = s.initClientSigners(); err != nil {
		return
	}
	// init sandbox manager
	if err = s.initSandboxManager(); err != nil {
		return
	}
	// init rpcConn
	if err = s.initRPCConn(); err != nil {
		return
	}
	// init sshServerConfig, must after host signer and rpcConn
	if err = s.initSSHServerConfig(); err != nil {
		return
	}
	// init listener
	if err = s.initListener(); err != nil {
		return
	}
	for {
		var c net.Conn
		if c, err = s.listener.Accept(); err != nil {
			if isClosedError(err) {
				err = nil
			}
			return
		}
		go s.handleConnection(c)
	}
	return
}

func (s *SSHD) handleConnection(c net.Conn) {
	var err error
	defer func() {
		if err != nil {
			log.Println(err)
		}
	}()
	// variables
	var sconn *ssh.ServerConn
	var nchan <-chan ssh.NewChannel
	var rchan <-chan *ssh.Request
	// upgrade connection to ssh connection
	if sconn, nchan, rchan, err = ssh.NewServerConn(c, s.sshServerConfig); err != nil {
		log.Println("bunkersshd: failed to upgrade ssh connection:", err)
		return
	}
	defer sconn.Close()
	if sconn.Permissions.Extensions[keyMode] == modeLv1 {
		err = s.handleLv1Connection(sconn, nchan, rchan)
	} else {
		err = s.handleLv2Connection(sconn, nchan, rchan)
	}
	return
}

func (s *SSHD) updateSandboxPublicKey(sb sandbox.Sandbox, account string) (err error) {
	var ak string
	if ak, err = sb.GetSSHPublicKey(); err != nil {
		return
	}
	var pk ssh.PublicKey
	if pk, _, _, _, err = ssh.ParseAuthorizedKey([]byte(ak)); err != nil {
		return
	}
	fp := ssh.FingerprintSHA256(pk)
	ks := types.NewKeyServiceClient(s.rpcConn)
	_, err = ks.CreateKey(context.Background(), &types.CreateKeyRequest{
		Name:        "sandbox",
		Account:     account,
		Fingerprint: fp,
		Source:      types.KeySourceSandbox,
	})
	return
}

func (s *SSHD) updateSandboxSSHConfig(sb sandbox.Sandbox, account string) (err error) {
	rs := types.NewGrantServiceClient(s.rpcConn)
	var riRes *types.ListGrantItemsResponse
	if riRes, err = rs.ListGrantItems(context.Background(), &types.ListGrantItemsRequest{Account: account}); err != nil {
		return
	}
	se := make([]sandbox.SSHEntry, 0)
	for _, ri := range riRes.GrantItems {
		// skip the special tunnel user
		if ri.User == types.GrantUserTunnel {
			continue
		}
		se = append(se, sandbox.SSHEntry{
			Name: fmt.Sprintf("%s-%s", ri.Hostname, ri.User),
			Host: s.opts.SandboxEndpoint,
			Port: uint(s.opts.Port),
			User: fmt.Sprintf("%s@%s", ri.User, ri.Hostname),
		})
	}
	_, _, err = sb.ExecScript(sandbox.ScriptSeedSSHConfig(se))
	return
}

func (s *SSHD) handleLv1Connection(sconn *ssh.ServerConn, ncchan <-chan ssh.NewChannel, grchan <-chan *ssh.Request) (err error) {
	account := sconn.Permissions.Extensions[keyAccount]
	log.Println("new account:", account)
	go func() {
		for req := range grchan {
			log.Println("Global Request:", req.Type)
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}()
	// find or create the sandbox
	var sb sandbox.Sandbox
	if sb, err = s.sandboxManager.FindOrCreate(account); err != nil {
		return
	}
	// update public from sandbox /root/.ssh/id_rsa
	if err = s.updateSandboxPublicKey(sb, account); err != nil {
		return
	}
	// update sandbox /root/.ssh/config
	if err = s.updateSandboxSSHConfig(sb, account); err != nil {
		return
	}
	// handle new channels
	for nc := range ncchan {
		log.Println("New Channel:", nc.ChannelType())
		log.Println("Extra Data:", nc.ExtraData())

		// if channel type is 'direct-tcpip'
		if nc.ChannelType() == channelTypeDirectTCPIP {
			// extract target hostname and target port
			var pl DirectTCPIPExtraData
			if pl, err = decodeDirectTCPIPExtraData(nc.ExtraData()); err != nil {
				log.Println(err)
				nc.Reject(ssh.UnknownChannelType, "failed to decode 'direct-tcpip' extra data")
				continue
			}
			// find the node
			ns := types.NewNodeServiceClient(s.rpcConn)
			var nRes *types.GetNodeResponse
			if nRes, err = ns.GetNode(context.Background(), &types.GetNodeRequest{Hostname: pl.Host}); err != nil {
				nc.Reject(ssh.ConnectionFailed, "host not found")
				continue
			}
			// check __tunnel__ user permission with given node
			rs := types.NewGrantServiceClient(s.rpcConn)
			var cRes *types.CheckGrantResponse
			if cRes, err = rs.CheckGrant(context.Background(), &types.CheckGrantRequest{
				Account:  account,
				User:     types.GrantUserTunnel,
				Hostname: pl.Host,
			}); err != nil {
				nc.Reject(ssh.ConnectionFailed, "failed to check permission")
				continue
			}
			if !cRes.Ok {
				nc.Reject(ssh.ConnectionFailed, "no permission")
				continue
			}
			var c ssh.Channel
			var crchan <-chan *ssh.Request
			if c, crchan, err = nc.Accept(); err != nil {
				log.Println("failed to accept new channel:", err)
				continue
			}
			go ssh.DiscardRequests(crchan) // no channel-level requests for 'direct-tcpip'
			go s.handleChannelDirectTCPIP(c, nRes.Node.Hostname, int(pl.Port))
		} else if nc.ChannelType() == channelTypeSession {
			var c ssh.Channel
			var crchan <-chan *ssh.Request
			if c, crchan, err = nc.Accept(); err != nil {
				log.Println("failed to accept new channel:", err)
				continue
			}
			go s.handleChannelSession(c, crchan, account)
		} else {
			nc.Reject(ssh.UnknownChannelType, "only channel type 'session' and 'direct-tcpip' is allowed")
			continue
		}
	}
	log.Println("finished")
	return
}

func (s *SSHD) handleChannelDirectTCPIP(c ssh.Channel, host string, port int) {
	// TODO: implement it
	c.Write([]byte(fmt.Sprintf("TARGET: %s:%d\n", host, port)))
	c.Write([]byte("Bye\n"))
	c.CloseWrite()
	c.Close()
}

func (s *SSHD) handleChannelSession(c ssh.Channel, crchan <-chan *ssh.Request, account string) {
	// TODO: implement it
}

func (s *SSHD) handleLv2Connection(sconn *ssh.ServerConn, nchan <-chan ssh.NewChannel, rchan <-chan *ssh.Request) (err error) {
	// no global requests is allowed in lv2 connection
	go ssh.DiscardRequests(rchan)
	return
}

func (s *SSHD) Shutdown() {
	if s.listener != nil {
		s.listener.Close()
	}
}
