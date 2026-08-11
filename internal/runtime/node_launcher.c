#define _POSIX_C_SOURCE 200809L

#include <stdlib.h>
#include <unistd.h>

extern char **environ;

int main(int argc, char **argv) {
  (void)argc;
  if (setenv("LD_LIBRARY_PATH", "/opt/helmr/runtime/lib", 1) != 0 ||
      setenv("NODE_OPTIONS",
             "--require=/opt/helmr/runtime/helmr/loader_env.cjs", 1) != 0) {
    static const char message[] = "helmr: prepare managed Node environment failed\n";
    (void)write(STDERR_FILENO, message, sizeof(message) - 1);
    return 127;
  }
  execve("/opt/helmr/runtime/bin/node.real", argv, environ);
  static const char message[] = "helmr: start managed Node failed\n";
  (void)write(STDERR_FILENO, message, sizeof(message) - 1);
  return 127;
}
