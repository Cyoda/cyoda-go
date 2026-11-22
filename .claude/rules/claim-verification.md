# Claim verification

A statement about this codebase is checked against this codebase, in the turn it
is made. Applies to: filing an issue, writing a PR body or commit message,
relaying a review or audit finding, briefing a subagent, and telling the human
that something is or is not the case.

- **Read the source, cite it `file:line`.** Not the summary, not the ledger, not
  what an earlier session concluded, not the agent report that mentioned it.
- **A reviewer's or auditor's note is an input, not a finding.** Verify its
  premise before acting on it. Escalating one unread is how a line of context
  becomes an open decision that was never open, and how a note filed as "raised
  for completeness" becomes a ticket someone has to argue their way out of.
- **A negative claim needs the search that shows it.** "Not documented", "not
  handled", "not tested", "nowhere in the tree" — run it, and show the empty
  output. These are the claims most often wrong, because nothing contradicts
  them until someone looks.
- **Restating someone else's words is itself a claim.** Counts, severities and
  scope are re-read from the source rather than recalled.

The failure this prevents is not being wrong. It is spending the reader's time
disproving something that one read of the code would have settled first.
