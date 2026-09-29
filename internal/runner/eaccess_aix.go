package runner

// atEaccess is AT_EACCESS from AIX's <fcntl.h>; golang.org/x/sys/unix does
// not define it for AIX.
const atEaccess = 0x1
