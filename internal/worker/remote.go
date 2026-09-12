package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"relaydock/internal/protocol"
	"relaydock/internal/remote"
	"relaydock/internal/transport"
)

var RemoteTools = []protocol.Tool{
	{Name: "remote.exec", Description: "Start an independent noninteractive shell command"},
	{Name: "remote.status", Description: "Read shell job status and incremental stdout/stderr"},
	{Name: "remote.cancel", Description: "Cancel a live shell job using its retained process handle"},
}

func (w *Worker) executeRemote(ctx context.Context, c protocol.Call) protocol.Result {
	if w.shell == nil {
		return protocol.Fail(c.ID, "disabled", "remote shell is not enabled on this Worker")
	}
	var a protocol.RemoteArgs
	d := json.NewDecoder(bytes.NewReader(c.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return protocol.Fail(c.ID, "invalid", err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return protocol.Fail(c.ID, "invalid", "trailing arguments")
	}
	var s protocol.RemoteSnapshot
	var err error
	switch c.Tool {
	case "remote.exec":
		if a.Cursor != "" || a.Limit != 0 {
			return protocol.Fail(c.ID, "invalid", "exec does not accept output cursors")
		}
		s, err = w.shell.Start(ctx, c.ID, a)
	case "remote.status", "remote.cancel":
		if a.Command != "" || a.CWD != "" || a.TimeoutMS != 0 {
			return protocol.Fail(c.ID, "invalid", "status/cancel cannot execute commands")
		}
		if c.Tool == "remote.status" {
			s, err = w.shell.Status(a.JobID, a.Cursor, a.Limit)
		} else {
			if a.Cursor != "" || a.Limit != 0 {
				return protocol.Fail(c.ID, "invalid", "cancel does not accept output cursors")
			}
			s, err = w.shell.Cancel(a.JobID)
		}
	default:
		return protocol.Fail(c.ID, "unsupported", "unknown remote tool")
	}
	if err != nil {
		code := "execution"
		if errors.Is(err, remote.ErrUncertain) || s.Job.Status == "unknown" {
			code = "unknown"
		}
		r := protocol.Fail(c.ID, code, err.Error())
		if s.Job.ID != "" {
			r.Data = protocol.JSON(s)
		}
		return r
	}
	return protocol.OK(c.ID, s)
}
func (w *Worker) sendRemoteEvents(p *transport.Peer) error {
	all, err := w.db.List("remote_outbox")
	if err != nil {
		return err
	}
	for i, id := range keys(all) {
		if i >= 256 {
			break
		}
		var s protocol.RemoteSnapshot
		if err = json.Unmarshal(all[id], &s); err != nil {
			return err
		}
		if err = p.Send(protocol.Wrap("remote_event", id, s)); err != nil {
			return err
		}
	}
	return nil
}
