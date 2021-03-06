package cmd

import (
	"fmt"

	"github.com/adnsio/dotd/pkg/server"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

//nolint:gochecknoglobals
var serverCmd = &cobra.Command{
	Use: "server",
	Aliases: []string{
		"serve",
	},
	Short: "Starts the DNS server",
	Long:  "Starts DotD DNS server, set it as primary resolver to start using it.",
	Run:   runServer,
}

//nolint:gochecknoinits
func init() {
	serverCmd.Flags().StringP("address", "a", "[::1]:53", "listening address")
	serverCmd.Flags().StringSliceP("upstreams", "u", []string{"https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query"}, "upstream addresses")
	serverCmd.Flags().StringSlice("blocklist", []string{}, "blocked domains")
	serverCmd.Flags().StringToString("resolve", map[string]string{}, "custom resolve list")

	if err := viper.BindPFlags(serverCmd.Flags()); err != nil {
		log.Fatal().Err(fmt.Errorf("viper: %w", err)).Send()
	}
}

func runServer(_ *cobra.Command, _ []string) {
	address := viper.GetString("address")
	upstreams := viper.GetStringSlice("upstreams")
	blocklist := viper.GetStringSlice("blocklist")
	blockregex := viper.GetStringSlice("blockregex")
	resolve := viper.GetStringMapString("resolve")

	server, err := server.New(
		address,
		upstreams,
		blocklist,
		blockregex,
		resolve,
	)
	if err != nil {
		log.Fatal().Err(fmt.Errorf("dotd: %w", err)).Send()
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatal().Err(fmt.Errorf("dotd: %w", err)).Send()
	}
}
