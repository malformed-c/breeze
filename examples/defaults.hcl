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

  # Prefer the two above for a shared host. A quota is right when you're budgeting
  # rather than sharing: unlike a weight it bites even when nothing else wants the
  # CPU, so a stage that INHERITS this one is held to 14 cores on a 28-core box
  # even while the other 14 sit idle.
  #
  # It is a DEFAULT, not a ceiling on what a stage may ask for. A stage naming its
  # own cpu_quota replaces this value and may name a larger one — measured on the
  # machine this example came from: a stage declaring "2800%" under this exact
  # "1400%" ran with cpu.max = 2800000 100000, i.e. all 28 cores, with every parent
  # cgroup at max. An earlier version of this comment said it would "leave 14 cores
  # idle at all times", which quietly moved from "regardless of contention" to
  # "regardless of configuration" and is the one sentence here someone doing
  # capacity arithmetic would have acted on.
  # cpu_quota = "1400%"
}
