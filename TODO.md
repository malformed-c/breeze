# TODO

Open items for breeze. One heading per item; keep the evidence with the item so
a reader does not have to reconstruct why it is here.

---

## A stage runs on the bare host, so unrelated host state can fail a commit

**Status:** open. Reported 2026-08-08 by trail-main, from a real red stage.
**Severity:** low for correctness, high for trust — the failure is attributed to
the commit, and it is not the commit's.

### What happened

`breeze start stage periapsis test bdd2b451` went red on engix99. The stage does
the right thing with the *filesystem* — its own stderr says so:

```
[test] testing bdd2b45177e7afec2ecbfa101e73197aeaff878d in a pinned worktree (not the working tree)
Preparing worktree (detached HEAD bdd2b451)
```

The single failure was:

```
--- FAIL: TestP2PListener_H3MTLSEnforcement (0.00s)
    p2p_listener_test.go:238: newP2PListener: listen udp P2P :33509:
        listen udp :33509: bind: address already in use
```

`kdeconnectd` (pid 22821) holds UDP `*:33509` on engix99. Nothing about the
commit under test touches that package — `bdd2b451` changes `cmd/perigeos` only.

### Root cause, and it is not breeze's

The periapsis test helper picks its port by binding **TCP** `:0` and reusing the
number for a listener that binds **both** TCP and UDP:

```go
l, err := net.Listen("tcp", ":0")     // p2p_listener_test.go:369
return l.Addr().(*net.TCPAddr).Port
```

A free TCP port is not a free UDP port. Measured on engix99 with kdeconnectd
holding UDP 33509:

```
net.Listen       tcp :33509 -> OK      (freeTCPPort would return 33509)
net.ListenPacket udp :33509 -> listen udp :33509: bind: address already in use
```

So the collision is a latent flake in periapsis that fires only when some
UDP-only process happens to hold the number the TCP probe hands out. **That fix
belongs in periapsis**, not here (probe UDP too, or bind both before returning).

*Instrument note:* the first positive control I ran reported UDP 33509 as
bindable, because it set `SO_REUSEADDR` and kdeconnectd sets it too. Go's
`net.ListenPacket` does not. The numbers above are from the re-run without it —
the first version of this evidence would have disproved the actual cause.

### Why it is nevertheless a breeze item

The stage isolates the *worktree* and not the *host*. Ports, and any other
machine-global resource a test grabs, are shared with whatever the developer is
running — a phone-sync daemon, in this case. That means:

- A red stage does not reliably mean "this commit is bad", which is the one
  thing a gate is for.
- It is **not reproducible across machines**: the same commit passes on a host
  where nothing holds that port, so the next person cannot confirm it.
- It is silent about which kind of failure it was. Nothing in the stage output
  distinguishes "the code is broken" from "the host was busy".

### Options, unranked — this needs a decision, not a patch

1. **Leave it.** Host-global collisions are the test suite's problem, and
   periapsis should fix its helper. Cheapest, and arguably correct: breeze is
   not a sandbox and does not claim to be. The cost is that every consumer
   re-learns this the way I did.
2. **Run the stage command in a private network namespace** (`unshare -n`, or a
   configurable wrapper in `ci/pipeline.hcl`). Removes the whole class. Breaks
   any stage that legitimately needs host networking — for periapsis's `test`
   stage that is probably fine, for `deploy` it certainly is not, so it would
   have to be per-stage and opt-in.
3. **Retry a failed stage once and report the disagreement.** Cheap, catches
   flakes generally rather than this class specifically, but a pass-on-retry is
   evidence of nondeterminism that should be surfaced loudly, not smoothed over.
4. **Record host context with the stage result** (listening ports, load) so a
   red stage can at least be diagnosed after the fact without re-running it.
   Complements any of the above.

My weak preference is (1) plus (4): fix the helper where the bug is, and make
breeze's record good enough that the next such failure is diagnosable from the
stage result alone rather than from a live host that has since changed.

### Follow-up

- [ ] periapsis: make the port helper probe UDP as well as TCP.
- [ ] breeze: decide among the options above; this file is the report, not the
      decision.
