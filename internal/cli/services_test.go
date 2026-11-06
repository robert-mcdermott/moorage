// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

func TestLogsOfAServiceHaveOnlyTail(t *testing.T) {
	code, _, errOut := run(t, "logs", "--app", "shop", "--service", "db", "--follow")
	if code == 0 || !strings.Contains(errOut, "--service has only --tail") {
		t.Errorf("%d %s", code, errOut)
	}
	code, _, errOut = run(t, "services", "upgrade", "db", "--app", "shop", "--finish")
	if code == 0 || !strings.Contains(errOut, "say --yes") {
		t.Errorf("%d %s", code, errOut)
	}
	code, _, errOut = run(t, "services", "upgrade", "db", "--app", "shop", "--back", "--finish")
	if code == 0 || !strings.Contains(errOut, "say --to VERSION") {
		t.Errorf("%d %s", code, errOut)
	}
}
