# Machine-level resource limits for ONE breeze daemon: copy to that daemon's state
# directory as defaults.hcl (`<repo>/.git/breeze/defaults.hcl`, or
# `$BREEZE_DIR/defaults.hcl`), then `breeze restart daemon` to load it.
#
# This is daemon policy, not pipeline config. It applies under EVERY command the
# daemon runs — every stage, every pre_gate/post_action hook, in every pipeline,
# including ones registered before this file existed and ones registered through
# the raw JSON path that never saw HCL. A pipeline or stage can override any field
# it names; whatever it doesn't name falls back to here.
#
# The case this exists for: CI sharing a host with something that must stay
# responsive (a control plane, a database, another agent's build). Weights are the
# right tool there — unlike a quota, a weight costs nothing while the box is idle
# and only decides who yields when it isn't.
#
# `breeze status` prints what's actually in effect. A malformed value here makes
# the daemon refuse to start (with the reason in its log) rather than quietly
# running everything unlimited.

resource_limits {
  cpu_weight  = 20      # 1-10000, default 100 — everything breeze runs yields to the rest of the box
  memory_high = "4G"    # soft ceiling: throttle and reclaim, don't OOM-kill
  tasks_max   = 1024

  # Prefer the two above for a shared host. A hard cap is right when you're
  # budgeting rather than sharing — it applies even when nothing else wants the
  # CPU, so on a 28-core box this would leave 14 cores idle at all times:
  # cpu_quota = "1400%"
}
