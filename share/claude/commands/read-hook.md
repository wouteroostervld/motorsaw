---
description: Switch motorsaw's read hook for this project (deny, context, off, default), or show its mode
argument-hint: "[deny|context|off|default]"
allowed-tools: Bash(motorsaw hook mode:*)
disable-model-invocation: true
---

Run `motorsaw hook mode $ARGUMENTS` with the Bash tool, exactly so, and report
its one line of output to the user, nothing more. Do not change any setting
yourself.

The modes: `deny` refuses an agent's first whole read of a large source file
and hands it the outline; `context` adds the outline and lets the read go
ahead; `off` does nothing; `default` goes back to what `MOTORSAW_READ_HOOK`
in the settings says. With no argument it shows the mode in force. It holds
for the whole project, agents in its worktrees included, from the next Read on.
