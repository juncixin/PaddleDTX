// Copyright (c) 2021 PaddlePaddle Authors. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package challenge

import (
	"github.com/spf13/cobra"
)

const timeTemplate = "2006-01-02 15:04:05"

var (
	host        string
	id          string
	owner       string
	storageNode string
	fileID      string
	start       string
	end         string
	limit       uint64
	list        int8
)

// rootCmd represents the task command
var rootCmd = &cobra.Command{
	Use:   "challenge",
	Short: "A command helps to manage tasks",
}

func RootCmd() *cobra.Command {
	return rootCmd
}
func init() {
	rootCmd.PersistentFlags().StringVar(&host, "host", "", "server address of xuper db")

	rootCmd.MarkPersistentFlagRequired("host")
}
