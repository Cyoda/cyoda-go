# Callout failover and fencing a replaced compute node — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's callout failover and fencing model. cyoda-go is the authoritative
implementation.

## Fencing a replaced compute node

The transaction token names the callout it was issued for and a number that
rises each time cyoda gives that callout's work to another compute node. A
request presented under the token is admitted only under the callout's
current number, checked after the transaction's own tenant check. Every
joined request then takes the transaction's lock for its whole length and is
checked again under it, so the check that decides who may touch the
transaction and the lock that protects it are the same one. Before the next
compute node gets the work, or before the engine carries on past a callback
that was itself waiting on a callout, the owner waits for any request already
in progress to finish. A request refused by the check answers `410
CALLOUT_SUPERSEDED`; once the transaction itself has ended, the answer is
`404 TRANSACTION_NOT_FOUND` instead.

A compute node that receives `CALLOUT_SUPERSEDED` must stop working on that
request: nothing further it sends under that token is accepted, and any
answer it does send is discarded. A request that was already reading or
writing when the work moved on is allowed to finish and is answered
normally — it is not interrupted, and the owner's wait above is what makes
that safe.

## Joined-request wire behaviour Cloud must replicate

These follow from taking the transaction's lock for every joined request
(reads included), not only entity writes, and from reading each request whole
before the lock is taken:

- A refused gRPC **server-streaming** joined request
  (`EntityManageCollection`, `EntitySearchCollection`) carries the request id
  in its error envelope even for `TRANSACTION_NOT_FOUND` and `FORBIDDEN`, not
  only for `CALLOUT_SUPERSEDED` — the request message is received, and its id
  therefore known, before the pass is judged. A refusal produced instead while
  resolving which node should serve the call — a malformed or expired pass,
  before the message is read at all — carries no id.
- An over-size body on a joined HTTP request is refused with `413`, the same
  as an unjoined one: the body is read into memory, under the same size cap a
  handler would apply, before the lock is taken, so the join layer never
  accepts a body a handler would reject.
- A held gRPC server-streaming response is not all-or-nothing: if the handler
  fails partway through a joined chunked collection, the frames already
  produced are sent before the handler's error is returned, exactly as an
  unjoined stream would deliver them.

## Cloud alignment

For Cloud to stay aligned:

1. A callout's fencing number must be tracked the same way: it rises only on
   the way to giving the work to another compute node, never on a plain retry
   by the same one, and a request is admitted only under the current number.
2. The tenant check on the transaction must run before the fencing check, so
   that a stolen or forged token tells another tenant nothing about which
   callouts exist.
3. Every request joined to a transaction — a read as much as a write — must
   serialise on that transaction, and the owner must wait for one in progress
   before moving the callout on or committing past it.
4. The three wire behaviours above are what a client written against either
   server should be able to rely on identically.
