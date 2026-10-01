# Reviewing a spec with a human

The human reads the spec in a browser, selects text, comments, and approves; you receive
the comments while you keep working, change the file, and reply. Every step below exists
because skipping it broke a real review.

## 1. Serve

```bash
cd <the repo that holds the spec file>      # pin write-back resolves the file from here
agtrack spec serve <spec> --as <human-name> --idle 2h
```

- It blocks: run it as a **background** command and read the URL from its output.
- `--as` is who browser actions are attributed to (default `human`). It must differ from
  your actor (serve refuses otherwise), or `--actor-not self` would hide the human's comments.
- The link carries a capability token; the server binds 127.0.0.1 only and exits after the
  idle timeout. Give the human the **whole** URL and also write it to a file - terminals
  wrap long lines and a truncated token gives `{"error":"forbidden","message":"missing or
  wrong token"}`. After the first load a session cookie keeps reloads working.
- A new `serve` means a new port and token: always hand over the new link.
- Rebuilt the binary or changed the UI? Stop the old server and start a new one; a running
  server keeps serving the assets it was built with.

## 2. Receive

```bash
agtrack spec watch <spec> --actor-not self --after <cursor> --timeout 29m
```

- Run it so that **each output line notifies you** (Claude Code: the Monitor tool). A plain
  background shell only notifies when the process exits - the comments sit in its output
  file unseen until the human asks why you are not answering.
- Monitors expire (30 minutes in Claude Code): keep `--timeout` just under the limit and
  re-arm on every expiry with the cursor from the `TERMINAL` line, even when nothing
  happened - the human may come back hours later.
- Release mode `manual` (default): comments are held until the human clicks **Send to
  agent** or approves; they then arrive together with a `spec-released` line.
  `agtrack config set review.release auto` delivers each comment as it is written.
- The release expands every held note, **including your own earlier replies**, and
  `--actor-not self` does not filter those (the release is the human's event): skip lines
  authored by you.
- `spec await <spec> --until approval|comments|release` is the single-result form. Without
  `--after` it starts from durable state: an existing, non-stale approval returns at once;
  with `--until comments`, released unresolved comments return 10 at once.
- Missed it live? `agtrack spec notes <spec> --open` lists everything; nothing is lost.

## 3. Answer each comment

1. `agtrack spec notes <spec> --open` - ids, kinds, the quoted text.
2. Edit the markdown. The human's comments have already **pinned** the sections they
   touched (one `<!-- agtrack:id ... -->` line each) - those diffs are expected; leave them.
3. **Commit only the spec file** (`git commit -- path/to/spec.md`): other work may be in
   progress in the same tree. `spec sync` refuses a file with uncommitted changes (exit 4).
4. `agtrack spec sync <spec>` - imports a new revision. Pins create revisions too, so the
   number jumps; that is normal.
5. `agtrack note reply <id> "what changed, in which revision"`. Where you chose specifics
   the human did not ask for (frameworks, limits, defaults), say so and invite a change.
6. **Do not resolve the human's notes.** Leave them open for the human to resolve.
7. Tell the human what changed; the page refreshes itself.

A question the spec does not answer: answer it in the reply, put the decision in the spec,
and leave the thread open for confirmation rather than deciding silently. A quote shown as
"no longer present" after your edit is expected when you rewrote that sentence.

## 4. Approval

- **Approve / Ready to build** records an approval naming the revision, with optional build
  notes (guidance that is not spec text). Read the approval note: it may limit what to do
  ("don't build, just acknowledge").
- An approval goes **stale** when the content changes afterwards; a pin-only revision does
  not make it stale. `resume` and `await` report stale approvals as stale, never as approved.
- After approval, stop the server - no mutating server should outlive the review.

## 5. Linking tasks

```bash
agtrack task add "Item creation rules" -p <project> --spec <spec>#items__creation --step "..." --idem K
agtrack spec link <spec>#lists__deletion <task>      # a further section for the same task
agtrack spec coverage <spec>                          # sections with text and no implementing task
```

- Linking pins the section in the file: commit the spec file afterwards.
- `task add` with **two** `--spec` sections that both still need a pin fails ("file has
  changed since vN") and rolls back; use one `--spec`, then `spec link` for the rest.
- `task show` does not list linked sections; use `spec toc` (task ids per section) or
  `spec check <section>`.
- `task done` re-stamps what the task agreed to; `spec stale` then lists tasks whose
  sections changed later. Nothing reopens automatically - reopening is a decision.
