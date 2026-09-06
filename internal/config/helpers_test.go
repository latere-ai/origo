// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import "os"

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o644) }
