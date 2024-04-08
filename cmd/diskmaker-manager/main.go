package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "diskmaker",
	Short: "Used to start the diskmaker daemon for the local-storage-operator",
}

// zhou: DaemonSet diskmaker-manager "assets/templates/diskmaker-manager-daemonset.yaml" works in this mode.
//       reconcile "LocalVolumeSet", API for OCS 4.6+

var managerCmd = &cobra.Command{
	Use:   "lv-manager",
	Short: "Used to start the controller-runtime manager that owns the LocalVolumeSet controller",
	RunE:  startManager,
}

// zhou: handle "LocalVolume", legacy API for OCS 4.4/4.5

var lvDaemonCmd = &cobra.Command{
	Use:   "lv-controller",
	Short: "Used to start the controller-runtime manager that owns the LocalVolume controller",
	RunE:  startManager,
}

// zhou: DaemonSet diskmaker-discovery "assets/templates/diskmaker-discovery-daemonset.yaml" works in this mode.
//       Timer triggered repeatly get "LocalVolumeDiscovery" and create/update "LocalVolumeDiscoveryResult".
//       API for OCS 4.6+

var discoveryDaemonCmd = &cobra.Command{
	Use:   "discover",
	Short: "Used to start device discovery for the LocalVolumeDiscovery CR",
	RunE:  startDeviceDiscovery,
}

// zhou: compiled as binary "diskmaker"

func main() {
	rootCmd.AddCommand(lvDaemonCmd)
	rootCmd.AddCommand(managerCmd)
	rootCmd.AddCommand(discoveryDaemonCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
