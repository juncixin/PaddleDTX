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

package crypto

// PadStart pad bytes into to specific length
// padding zero at the begining of the bytes
func PadStart(s []byte, newLen int) []byte {
	if len(s) >= newLen {
		return s
	}

	t := make([]byte, newLen-len(s), newLen)
	return append(t, s...)
}
