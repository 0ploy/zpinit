package supervisor

import "context"

// Start and Stop are the context-free command helpers, available to
// tests only. They deliberately do NOT exist in the production build:
// both block forever once the Run goroutine has exited (the buffered
// cmds channel accepts the send, but the reply never comes), so every
// production path must use StartCtx/StopCtx. Keeping them out of the
// package's real surface removes the footgun rather than documenting
// it. Tests drive a Runner whose Run loop is always alive, where the
// blocking form is both safe and much less noisy.
func (r *Runner) Start() { _ = r.sendCtx(context.Background(), cmdStart) }
func (r *Runner) Stop()  { _ = r.sendCtx(context.Background(), cmdStop) }
