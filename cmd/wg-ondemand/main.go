package main

import (
	"context"
	"fmt"
	"net"

	"github.com/charmbracelet/log"
	"github.com/schidstorm/wg-ondemand/pkg/aws"
	"github.com/schidstorm/wg-ondemand/pkg/hetzner"
	"github.com/schidstorm/wg-ondemand/pkg/provision"
	"github.com/spf13/cobra"
)

func main() {
	configureLogging()

	cmd := &cobra.Command{
		Use: "wg-ondemand",
	}

	cmd.AddCommand(provisionCmd())
	cmd.AddCommand(deProvisionCmd())
	cmd.AddCommand(regionsCmd())

	err := cmd.Execute()
	if err != nil {
		panic(err)
	}

}

func configureLogging() {
	log.Default().SetTimeFormat("15:04:05")
}

func provisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "deploy",
	}

	publicKey := cmd.Flags().StringP("public-key", "k", "", "Client public key")
	wgPort := cmd.Flags().Uint16P("port", "p", 51820, "Wireguard port")
	region := cmd.Flags().StringP("region", "r", "", "AWS region")
	id := cmd.Flags().StringP("id", "i", "wg-ondemand", "Provision ID")
	provisionerType := cmd.Flags().StringP("type", "t", "aws", "Provisioner type")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		provisioner, err := createAndInitProvisioner(*provisionerType)
		if err != nil {
			log.Error("Failed to initialize provisioner", "err", err)
			return err
		}

		log.Info("Provision", "type", *provisionerType)
		res, err := provisioner.Provision(context.Background(), *id, provision.ProvisionArguments{
			ClientPublicKey: *publicKey,
			ClientWgIp:      net.ParseIP("172.30.0.2"),
			ServerWgIp:      net.ParseIP("172.30.0.1"),
			WgPort:          *wgPort,
			Type:            *provisionerType,
			Region:          *region,
		})
		if err != nil {
			log.Error("Failed to provision server", "err", err)
			return err
		}

		fmt.Printf(`
[Peer]
PublicKey = %s
AllowedIPs = 0.0.0.0/0
Endpoint = %s:%d
`, res.ServerPublicKey, res.ServerIP, *wgPort)

		return nil
	}

	return cmd
}

func deProvisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "delete",
	}

	region := cmd.Flags().StringP("region", "r", "", "AWS region")
	id := cmd.Flags().StringP("id", "i", "wg-ondemand", "Provision ID")
	provisionerType := cmd.Flags().StringP("type", "t", "aws", "Provisioner type")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		provisioner, err := createAndInitProvisioner(*provisionerType)
		if err != nil {
			log.Error("Failed to initialize provisioner", "err", err)
			return err
		}

		return provisioner.DeProvision(context.Background(), *id, provision.DeProvisionArguments{
			Region: *region,
		})
	}

	return cmd
}

func regionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "regions",
	}

	provisionerType := cmd.Flags().StringP("type", "t", "aws", "Provisioner type")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		provisioner, err := createAndInitProvisioner(*provisionerType)
		if err != nil {
			log.Error("Failed to initialize provisioner", "err", err)
			return err
		}

		locations, err := provisioner.Locations(context.Background())
		if err != nil {
			log.Error("Failed to get locations", "err", err)
			return err
		}

		for _, loc := range locations {
			fmt.Printf("%s: %s, %s\n", loc.Key, loc.City, loc.Country)
		}

		return nil
	}

	return cmd
}

func createAndInitProvisioner(t string) (provision.Provisioner, error) {
	var provisioner provision.Provisioner
	switch t {
	case "aws":
		provisioner = &aws.AwsProvisioner{}
	case "hetzner":
		provisioner = &hetzner.HetznerProvisioner{}
	default:
		return nil, fmt.Errorf("unknown provisioner type: %s", t)
	}

	return provisioner, nil
}
