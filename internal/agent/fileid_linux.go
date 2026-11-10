package agent

import "golang.org/x/sys/unix"

// lookupOnly opens a folder to look names up in it and to say its inode,
// without the right to read it, as a database's 0700 data folder of
// another owner is (#30). With O_NOFOLLOW and O_DIRECTORY, a link is not a
// folder.
const lookupOnly = unix.O_PATH
