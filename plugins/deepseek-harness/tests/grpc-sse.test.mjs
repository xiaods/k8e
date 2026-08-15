// Pure-unit test for the /exec/stream SSE decoder (no harness, no gRPC).
import assert from 'node:assert/strict'
import { ExecSSEDecoder } from '@k8e/dsh-k8e-sandbox-client/grpc'

// pid frame is dropped; data + exit are emitted in order.
{
  const d = new ExecSSEDecoder()
  const events = d.push('data: {"pid":123}\n\ndata: hello\n\ndata: {"exit":0}\n\n')
  assert.deepEqual(events, [{ data: 'hello' }, { exit: 0 }])
}

// byte-by-byte feeding must reassemble events across arbitrary chunk splits.
{
  const d = new ExecSSEDecoder()
  const full = 'data: {"pid":1}\n\ndata: ab\n\ndata: {"exit":42}\n\n'
  const events = []
  for (const ch of full) events.push(...d.push(ch))
  assert.deepEqual(events, [{ data: 'ab' }, { exit: 42 }])
}

// a single newline inside the payload survives (the raw framing limitation is
// only a literal "\n\n" inside data, which would split the frame early).
{
  const d = new ExecSSEDecoder()
  assert.deepEqual(d.push('data: line1\nline2\n\ndata: {"exit":0}\n\n'), [
    { data: 'line1\nline2' },
    { exit: 0 },
  ])
}

// negative exit (killed by signal) is preserved.
{
  const d = new ExecSSEDecoder()
  assert.deepEqual(d.push('data: {"exit":-1}\n\n'), [{ exit: -1 }])
}

// a partial event is retained until the rest arrives.
{
  const d = new ExecSSEDecoder()
  assert.deepEqual(d.push('data: hel'), [])
  assert.deepEqual(d.push('lo\n\n'), [{ data: 'hello' }])
}

console.log('✔ grpc SSE decoder test passed')
