#define _POSIX_C_SOURCE 200809L

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#ifndef O_NOFOLLOW
#define O_NOFOLLOW 0
#endif

#ifndef HELMR_NODE_PATH
#define HELMR_NODE_PATH "/opt/helmr/runtime/bin/node"
#endif

#ifndef HELMR_PRELOAD_ARG
#define HELMR_PRELOAD_ARG "--import=file:///opt/helmr/runtime/helmr/preload.mjs"
#endif

extern char **environ;

static const char shim_header[] =
    "#!/opt/helmr/runtime/bin/helmr-sh\n"
    "exec /opt/helmr/runtime/bin/node "
    "--import=file:///opt/helmr/runtime/helmr/preload.mjs '";
static const char shim_footer[] = "' \"$@\"\n";

static const char *reserved_names[] = {
    "NODE_OPTIONS",
    "NODE_PATH",
    "NODE_EXTRA_CA_CERTS",
    "NODE_ICU_DATA",
    "SSL_CERT_FILE",
    "SSL_CERT_DIR",
    "OPENSSL_CONF",
    "OPENSSL_MODULES",
    "OPENSSL_ENGINES",
    "GCONV_PATH",
    "LOCPATH",
};

static void fail(const char *format, ...) {
  va_list arguments;

  fputs("helmr-sh: ", stderr);
  va_start(arguments, format);
  vfprintf(stderr, format, arguments);
  va_end(arguments);
  fputc('\n', stderr);
  exit(126);
}

static bool has_name(const char *entry, const char *name) {
  size_t length = strlen(name);
  return strncmp(entry, name, length) == 0 && entry[length] == '=';
}

static bool is_reserved(const char *entry) {
  if (strncmp(entry, "LD_", 3) == 0) {
    return true;
  }
  for (size_t index = 0;
       index < sizeof(reserved_names) / sizeof(reserved_names[0]); index++) {
    if (has_name(entry, reserved_names[index])) {
      return true;
    }
  }
  return false;
}

static char **runtime_environment(void) {
  static char gconv_path[] =
      "GCONV_PATH=/opt/helmr/runtime/lib/gconv";
  static char locale_path[] =
      "LOCPATH=/opt/helmr/runtime/lib/locale";
  static char openssl_conf[] =
      "OPENSSL_CONF=/opt/helmr/runtime/lib/openssl/openssl.cnf";
  static char openssl_modules[] =
      "OPENSSL_MODULES=/opt/helmr/runtime/lib/ossl-modules";
  static char *fixed[] = {
      gconv_path,
      locale_path,
      openssl_conf,
      openssl_modules,
  };
  size_t count = 0;

  while (environ[count] != NULL) {
    count++;
  }
  char **result = calloc(count + sizeof(fixed) / sizeof(fixed[0]) + 1,
                         sizeof(*result));
  if (result == NULL) {
    fail("allocate environment: %s", strerror(errno));
  }
  size_t output = 0;
  for (size_t index = 0; index < count; index++) {
    if (!is_reserved(environ[index])) {
      result[output++] = environ[index];
    }
  }
  for (size_t index = 0; index < sizeof(fixed) / sizeof(fixed[0]); index++) {
    result[output++] = fixed[index];
  }
  result[output] = NULL;
  return result;
}

static char *read_shim(const char *path, size_t *length) {
  const off_t max_shim_bytes =
      (off_t)(sizeof(shim_header) - 1) +
      (off_t)(4 * (PATH_MAX - 1)) +
      (off_t)(sizeof(shim_footer) - 1);
  int descriptor = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  if (descriptor < 0) {
    fail("open shim: %s", strerror(errno));
  }
  struct stat status;
  if (fstat(descriptor, &status) != 0) {
    fail("stat shim: %s", strerror(errno));
  }
  if (!S_ISREG(status.st_mode) || status.st_size < 0 ||
      status.st_size > max_shim_bytes) {
    fail("shim is not a bounded regular file");
  }
  size_t size = (size_t)status.st_size;
  char *contents = malloc(size + 1);
  if (contents == NULL) {
    fail("allocate shim: %s", strerror(errno));
  }
  size_t offset = 0;
  while (offset < size) {
    ssize_t count = read(descriptor, contents + offset, size - offset);
    if (count < 0 && errno == EINTR) {
      continue;
    }
    if (count <= 0) {
      fail("read shim: %s", count == 0 ? "unexpected EOF" : strerror(errno));
    }
    offset += (size_t)count;
  }
  char trailing;
  ssize_t count;
  do {
    count = read(descriptor, &trailing, 1);
  } while (count < 0 && errno == EINTR);
  if (count != 0) {
    fail("shim changed while reading");
  }
  if (close(descriptor) != 0) {
    fail("close shim: %s", strerror(errno));
  }
  contents[size] = '\0';
  *length = size;
  return contents;
}

static bool valid_target(const char *target, size_t length) {
  static const char root[] = "/opt/helmr/program/";
  if (length < sizeof(root) ||
      memcmp(target, root, sizeof(root) - 1) != 0 ||
      length + 1 > PATH_MAX) {
    return false;
  }
  size_t component = 0;
  for (size_t index = sizeof(root) - 1; index < length; index++) {
    unsigned char value = (unsigned char)target[index];
    if (value == '/' || index == length - 1) {
      if (index == length - 1 && value != '/') {
        component++;
      }
      if (component == 0 || component > 255) {
        return false;
      }
      size_t start = index + (value == '/' ? 0 : 1) - component;
      if ((component == 1 && target[start] == '.') ||
          (component == 2 && target[start] == '.' && target[start + 1] == '.')) {
        return false;
      }
      component = 0;
      continue;
    }
    if (value < 0x20 || value == 0x7f || value == '\\') {
      return false;
    }
    component++;
  }
  return component == 0;
}

static char *parse_target(const char *contents, size_t length) {
  size_t header_length = sizeof(shim_header) - 1;
  size_t footer_length = sizeof(shim_footer) - 1;
  if (length < header_length + footer_length ||
      memcmp(contents, shim_header, header_length) != 0) {
    fail("shim does not match the fixed launcher grammar");
  }
  char *target = malloc(PATH_MAX);
  if (target == NULL) {
    fail("allocate target: %s", strerror(errno));
  }
  size_t input = header_length;
  size_t output = 0;
  bool ended = false;
  while (input < length) {
    if (length - input == footer_length &&
        memcmp(contents + input, shim_footer, footer_length) == 0) {
      ended = true;
      break;
    }
    if (contents[input] == '\'') {
      static const char quote_escape[] = "'\\''";
      if (length - input < sizeof(quote_escape) - 1 ||
          memcmp(contents + input, quote_escape,
                 sizeof(quote_escape) - 1) != 0) {
        fail("shim contains an invalid quote");
      }
      if (output + 1 >= PATH_MAX) {
        fail("shim target is too long");
      }
      target[output++] = '\'';
      input += sizeof(quote_escape) - 1;
      continue;
    }
    if (output + 1 >= PATH_MAX) {
      fail("shim target is too long");
    }
    target[output++] = contents[input++];
  }
  if (!ended || !valid_target(target, output)) {
    fail("shim target is outside the managed Program");
  }
  target[output] = '\0';
  return target;
}

int main(int argc, char **argv) {
  if (argc < 2) {
    fail("missing shim path");
  }
  size_t shim_length;
  char *shim = read_shim(argv[1], &shim_length);
  char *target = parse_target(shim, shim_length);

  char **node_argv = calloc((size_t)argc + 2, sizeof(*node_argv));
  if (node_argv == NULL) {
    fail("allocate arguments: %s", strerror(errno));
  }
  node_argv[0] = (char *)HELMR_NODE_PATH;
  node_argv[1] = (char *)HELMR_PRELOAD_ARG;
  node_argv[2] = target;
  for (int index = 2; index < argc; index++) {
    node_argv[index + 1] = argv[index];
  }
  node_argv[argc + 1] = NULL;

  execve(HELMR_NODE_PATH, node_argv, runtime_environment());
  fail("execute managed Node: %s", strerror(errno));
}
