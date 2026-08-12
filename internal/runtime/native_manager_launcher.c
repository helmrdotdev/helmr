#define _POSIX_C_SOURCE 200809L

#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

extern char **environ;

static const char runtime_loader[] = "/opt/helmr/runtime/ld.so";
static const char runtime_library_path[] = "/opt/helmr/runtime/lib";
static const char bun_path[] = "/opt/helmr/manager/libexec/bun";

static void write_diagnostic(const char *message, size_t size) {
  ssize_t written = write(STDERR_FILENO, message, size);
  (void)written;
}

static const char *base_name(const char *path) {
  const char *name = path;
  for (const char *cursor = path; *cursor != '\0'; cursor++) {
    if (*cursor == '/') {
      name = cursor + 1;
    }
  }
  return name;
}

int main(int argc, char **argv) {
  if (argc < 1 || argv == NULL || argv[0] == NULL ||
      strcmp(base_name(argv[0]), "bun") != 0) {
    static const char message[] = "helmr: unsupported native manager launcher name\n";
    write_diagnostic(message, sizeof(message) - 1);
    return 127;
  }

  if ((size_t)argc > (SIZE_MAX / sizeof(char *)) - 4) {
    return 127;
  }
  char **launch_argv = calloc((size_t)argc + 4, sizeof(char *));
  if (launch_argv == NULL) {
    return 127;
  }
  launch_argv[0] = (char *)runtime_loader;
  launch_argv[1] = (char *)"--library-path";
  launch_argv[2] = (char *)runtime_library_path;
  launch_argv[3] = (char *)bun_path;
  for (int index = 1; index < argc; index++) {
    launch_argv[index + 3] = argv[index];
  }
  launch_argv[argc + 3] = NULL;

  execve(runtime_loader, launch_argv, environ);
  static const char message[] = "helmr: start managed native manager failed\n";
  write_diagnostic(message, sizeof(message) - 1);
  return 127;
}
