package hetzner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"strconv"
	"time"

	"net"

	"github.com/charmbracelet/log"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/schidstorm/wg-ondemand/pkg/provision"
	"golang.org/x/crypto/ssh"
)

const sshPort = 22

type HetznerProvisioner struct {
	client    *hcloud.Client
	privKey   ed25519.PrivateKey
	pubKeyPem string
}

func (p *HetznerProvisioner) Provision(ctx context.Context, id string, args provision.ProvisionArguments) (provision.ProvisionResult, error) {
	err := p.init()
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	sshKey, err := p.createSshKey(ctx, id)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	firewall, err := p.createOrUpdateFirewall(ctx, id, args.WgPort)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	_, err = p.createOrRecreateServer(ctx, id, args.Region, sshKey, *firewall)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	var server *hcloud.Server
	for {
		server, _, err = p.client.Server.GetByName(ctx, id)
		if err != nil {
			return provision.ProvisionResult{}, err
		}

		if server.Status == hcloud.ServerStatusRunning {
			break
		}

		time.Sleep(10 * time.Second)
	}

	for {
		res, err := p.runShell(ctx, server, "echo 1")
		if err == nil {
			break
		}

		log.Info("waiting for server to be ready", "res", string(res))
		time.Sleep(5 * time.Second)
	}

	outputParams, err := args.RunInitScript(ctx, func(script string) (string, error) {
		stdout, err := p.runShell(ctx, server, script)
		return string(stdout), err
	})
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	return provision.ProvisionResult{
		ServerIP:        server.PublicNet.IPv4.IP,
		ServerWgIp:      args.ServerWgIp,
		ServerPublicKey: string(outputParams.ServerWgPublicKey),
	}, nil
}

func (p *HetznerProvisioner) createSshKey(ctx context.Context, name string) (*hcloud.SSHKey, error) {
	sshKey, _, err := p.client.SSHKey.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}

	if sshKey != nil {
		p.client.SSHKey.Delete(ctx, sshKey)
	}

	sshKey, _, err = p.client.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      name,
		PublicKey: p.pubKeyPem,
	})
	return sshKey, err
}

func (p *HetznerProvisioner) createOrUpdateFirewall(ctx context.Context, name string, wgPort uint16) (*hcloud.Firewall, error) {
	_, netAny, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		return nil, err
	}

	firewall, _, err := p.client.Firewall.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}

	var rules = []hcloud.FirewallRule{
		{
			Direction:   hcloud.FirewallRuleDirectionIn,
			SourceIPs:   []net.IPNet{*netAny},
			Port:        pstr(strconv.FormatUint(uint64(wgPort), 10)),
			Protocol:    hcloud.FirewallRuleProtocolUDP,
			Description: pstr("Wireguard"),
		},
		{
			Direction:   hcloud.FirewallRuleDirectionIn,
			SourceIPs:   []net.IPNet{*netAny},
			Port:        pstr(strconv.FormatUint(uint64(sshPort), 10)),
			Protocol:    hcloud.FirewallRuleProtocolTCP,
			Description: pstr("SSH"),
		},
	}

	if firewall != nil {
		firewall.Rules = rules
		newFw, _, err := p.client.Firewall.Update(ctx, firewall, hcloud.FirewallUpdateOpts{})
		return newFw, err
	}

	firewallResult, _, err := p.client.Firewall.Create(ctx, hcloud.FirewallCreateOpts{
		Name:  name,
		Rules: rules,
	})

	return firewallResult.Firewall, err
}

func (p *HetznerProvisioner) createOrRecreateServer(ctx context.Context, id string, region string, sshKey *hcloud.SSHKey, firewall hcloud.Firewall) (*hcloud.Server, error) {
	server, _, err := p.client.Server.GetByName(ctx, id)
	if err != nil {
		return nil, err
	}

	if server != nil {
		_, _, err = p.client.Server.DeleteWithResult(ctx, server)
		if err != nil {
			return nil, err
		}
	}

	serverResp, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:  id,
		Image: &hcloud.Image{Name: "rocky-9"},
		PublicNet: &hcloud.ServerCreatePublicNet{
			EnableIPv4: true,
		},
		SSHKeys: []*hcloud.SSHKey{
			sshKey,
		},
		Location: &hcloud.Location{Name: region},
		ServerType: &hcloud.ServerType{
			Name: "cx22",
		},
		Firewalls: []*hcloud.ServerCreateFirewall{
			{
				Firewall: firewall,
			},
		},
	})

	return serverResp.Server, err
}

func (p *HetznerProvisioner) runShell(ctx context.Context, server *hcloud.Server, script string) ([]byte, error) {
	signer, err := ssh.NewSignerFromKey(&p.privKey)
	if err != nil {
		return nil, err
	}

	sshClient, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", server.PublicNet.IPv4.IP.String(), sshPort), &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return nil, err
	}
	defer sshClient.Close()

	session, err := sshClient.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	stdoutBuffer := new(bytes.Buffer)
	session.Stdout = stdoutBuffer
	stderrBuffer := new(bytes.Buffer)
	session.Stderr = stderrBuffer

	err = session.Start(script)
	if err != nil {
		log.Error("failed to start session", "err", err, "stderr", stderrBuffer.String())
		return nil, err
	}

	doneChan := make(chan error)

	go func() {
		doneChan <- session.Wait()
	}()

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-doneChan:
	}
	if err != nil {
		log.Error("failed to wait for session", "err", err, "stderr", stderrBuffer.String())
		return nil, err
	}

	return stdoutBuffer.Bytes(), nil
}

func (p *HetznerProvisioner) DeProvision(ctx context.Context, id string, args provision.DeProvisionArguments) error {
	err := p.init()
	if err != nil {
		return err
	}

	server, _, err := p.client.Server.GetByName(ctx, id)
	if err == nil && server != nil {
		_, _, err = p.client.Server.DeleteWithResult(ctx, server)
		if err != nil {
			return err
		}

	}

	sshKey, _, err := p.client.SSHKey.GetByName(ctx, id)
	if err == nil && sshKey != nil {
		_, err = p.client.SSHKey.Delete(ctx, sshKey)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *HetznerProvisioner) Locations(ctx context.Context) ([]provision.Location, error) {
	err := p.init()
	if err != nil {
		return nil, err
	}

	hetznerLocations, err := p.client.Location.All(ctx)
	if err != nil {
		return nil, err
	}

	var locations []provision.Location
	for _, loc := range hetznerLocations {
		locations = append(locations, provision.Location{
			Latitude:  loc.Latitude,
			Longitude: loc.Longitude,
			Country:   loc.Country,
			City:      loc.City,
			Key:       loc.Name,
		})
	}

	return locations, nil
}

func pstr(s string) *string {
	return &s
}

func (p *HetznerProvisioner) init() error {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return fmt.Errorf("HCLOUD_TOKEN not set")
	}
	p.client = hcloud.NewClient(hcloud.WithToken(token))

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}

	pubKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	p.pubKeyPem = string(ssh.MarshalAuthorizedKey(pubKey))
	p.privKey = priv

	return nil
}
