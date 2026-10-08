package agent

import (
	"net/http"

	"github.com/wes/jokku/internal/session"
)

// serveSession opens a session with one of this node's VMs for the control
// node (jokku enter, volume copies). The HTTP connection is upgraded and
// relayed to the guest agent, with the VM's token added to the request.
func (a *Agent) serveSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	known := false
	if st := a.Desired(); st != nil {
		for _, s := range st.Instances {
			known = known || s.ID == id
		}
	}
	if !known {
		http.Error(w, "this node does not run that instance", http.StatusNotFound)
		return
	}
	guest, token, err := a.Runtime.Session(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer guest.Close()
	conn, brw, err := session.Hijack(w)
	if err != nil {
		return
	}
	defer conn.Close()
	req, err := session.NewReader(brw.Reader, conn).ReadRequest()
	if err != nil {
		return
	}
	req.Token = token
	if err := session.New(guest).SendRequest(req); err != nil {
		return
	}
	session.Splice(session.ReadWriter{Reader: brw.Reader, Writer: conn}, guest)
}
