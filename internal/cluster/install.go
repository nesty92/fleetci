package cluster

import (
	_ "embed"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"
)

// installScript is GET /v1/install.sh's: a new machine with nothing but
// curl and a token gets fleetci, then joins and runs as a service.
//
//go:embed install.sh
var installScript string

// InstallLine is the line that runs the installer on a new machine, for a
// worker token: curl pins the controller's key, then the script checks
// fleetci by its sum.
func InstallLine(controller, pin, token string) string {
	return "curl -fsSk --pinnedpubkey " + pin + " https://" + withPort(controller) + "/v1/install.sh | sh -s -- " + controller + " " + token
}

func (c *Controller) handleInstallScript(w http.ResponseWriter, _ *http.Request) {
	pin, err := c.pki.Pin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var sum string
	for _, a := range c.artifacts {
		if a.Name == "fleetci" {
			sum = a.SHA256
		}
	}
	// The port workers dial an address without one at, as withPort does.
	w.Header().Set("Content-Type", "text/x-shellscript")
	_, _ = strings.NewReplacer("{{arch}}", runtime.GOARCH, "{{sum}}", sum, "{{pin}}", pin, "{{port}}", DefaultPort).WriteString(w, installScript)
}

// handleInstallBinary serves this controller's fleetci to a machine with a
// token good for a join; the token is not used up.
func (c *Controller) handleInstallBinary(w http.ResponseWriter, r *http.Request) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, err := ParseToken(raw)
	if !ok || err != nil {
		writeErr(w, http.StatusUnauthorized, errors.New("this needs a join token: Authorization: Bearer fci1.…"))
		return
	}
	if _, t, err := c.checkToken(tok.ID+"."+tok.Secret, ""); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	} else if t.Role != RoleWorker {
		writeErr(w, http.StatusForbidden, errors.New("a controller's token: join it with fleetci join -as-controller"))
		return
	}
	self, err := os.Executable()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	f, err := os.Open(self)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "fleetci", st.ModTime(), f)
}
