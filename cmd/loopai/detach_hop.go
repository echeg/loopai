package main

// detachHopArg, as the first argument, makes loopai the hop between the --detach launcher and the
// detached child: it starts the rest of its arguments as the child and exits at once. main checks it
// before flag parsing and before the version banner, because the hop's stdout is the run's console log.
//
// The hop exists so the child is never a descendant of the agent that ran the launcher for longer than
// the hop lives. T3 Code tracks the descendants of a Claude thread's CLI with a `ps` walk over parent
// pids once a second and SIGKILLs every one of them, the whole group of a group leader, when the CLI
// exits; a new session does not hide a process from that walk, a dead parent does. Once the hop exits
// the child is reparented to launchd or init, so the walk can reach it only within the hop's few
// milliseconds. The hop is loopai itself rather than `sh -c '"$@" &'`: a non-interactive shell starts
// an asynchronous command with SIGINT and SIGQUIT ignored, and an ignored disposition survives exec
// into every process the run starts.
const detachHopArg = "--detached-hop"
