package cmd

import (
	"github.com/agux/pachon/getd"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(getCmd)
}

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Get relevant stock data",
	Run: func(cmd *cobra.Command, args []string) {
		defer shutdownHook()
		getd.GetV2()
	},
}

func shutdownHook() {
	if r := recover(); r != nil {
		if er, hasError := r.(error); hasError {
			log.Printf("caught error:%+v, trying to cleanup...", er)
			getd.Cleanup()
		}
	}
}
