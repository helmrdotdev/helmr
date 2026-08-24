var __create = Object.create;
var __getProtoOf = Object.getPrototypeOf;
var __defProp = Object.defineProperty;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
function __accessProp(key) {
  return this[key];
}
var __toESMCache_node;
var __toESMCache_esm;
var __toESM = (mod, isNodeMode, target) => {
  var canCache = mod != null && typeof mod === "object";
  if (canCache) {
    var cache = isNodeMode ? __toESMCache_node ??= new WeakMap : __toESMCache_esm ??= new WeakMap;
    var cached = cache.get(mod);
    if (cached)
      return cached;
  }
  target = mod != null ? __create(__getProtoOf(mod)) : {};
  const to = isNodeMode || !mod || !mod.__esModule ? __defProp(target, "default", { value: mod, enumerable: true }) : target;
  for (let key of __getOwnPropNames(mod))
    if (!__hasOwnProp.call(to, key))
      __defProp(to, key, {
        get: __accessProp.bind(mod, key),
        enumerable: true
      });
  if (canCache)
    cache.set(mod, to);
  return to;
};
var __commonJS = (cb, mod) => () => (mod || cb((mod = { exports: {} }).exports, mod), mod.exports);

// node_modules/.bun/picomatch@4.0.4/node_modules/picomatch/lib/constants.js
var require_constants = __commonJS((exports, module) => {
  var WIN_SLASH = "\\\\/";
  var WIN_NO_SLASH = `[^${WIN_SLASH}]`;
  var DEFAULT_MAX_EXTGLOB_RECURSION = 0;
  var DOT_LITERAL = "\\.";
  var PLUS_LITERAL = "\\+";
  var QMARK_LITERAL = "\\?";
  var SLASH_LITERAL = "\\/";
  var ONE_CHAR = "(?=.)";
  var QMARK = "[^/]";
  var END_ANCHOR = `(?:${SLASH_LITERAL}|$)`;
  var START_ANCHOR = `(?:^|${SLASH_LITERAL})`;
  var DOTS_SLASH = `${DOT_LITERAL}{1,2}${END_ANCHOR}`;
  var NO_DOT = `(?!${DOT_LITERAL})`;
  var NO_DOTS = `(?!${START_ANCHOR}${DOTS_SLASH})`;
  var NO_DOT_SLASH = `(?!${DOT_LITERAL}{0,1}${END_ANCHOR})`;
  var NO_DOTS_SLASH = `(?!${DOTS_SLASH})`;
  var QMARK_NO_DOT = `[^.${SLASH_LITERAL}]`;
  var STAR = `${QMARK}*?`;
  var SEP = "/";
  var POSIX_CHARS = {
    DOT_LITERAL,
    PLUS_LITERAL,
    QMARK_LITERAL,
    SLASH_LITERAL,
    ONE_CHAR,
    QMARK,
    END_ANCHOR,
    DOTS_SLASH,
    NO_DOT,
    NO_DOTS,
    NO_DOT_SLASH,
    NO_DOTS_SLASH,
    QMARK_NO_DOT,
    STAR,
    START_ANCHOR,
    SEP
  };
  var WINDOWS_CHARS = {
    ...POSIX_CHARS,
    SLASH_LITERAL: `[${WIN_SLASH}]`,
    QMARK: WIN_NO_SLASH,
    STAR: `${WIN_NO_SLASH}*?`,
    DOTS_SLASH: `${DOT_LITERAL}{1,2}(?:[${WIN_SLASH}]|$)`,
    NO_DOT: `(?!${DOT_LITERAL})`,
    NO_DOTS: `(?!(?:^|[${WIN_SLASH}])${DOT_LITERAL}{1,2}(?:[${WIN_SLASH}]|$))`,
    NO_DOT_SLASH: `(?!${DOT_LITERAL}{0,1}(?:[${WIN_SLASH}]|$))`,
    NO_DOTS_SLASH: `(?!${DOT_LITERAL}{1,2}(?:[${WIN_SLASH}]|$))`,
    QMARK_NO_DOT: `[^.${WIN_SLASH}]`,
    START_ANCHOR: `(?:^|[${WIN_SLASH}])`,
    END_ANCHOR: `(?:[${WIN_SLASH}]|$)`,
    SEP: "\\"
  };
  var POSIX_REGEX_SOURCE = {
    __proto__: null,
    alnum: "a-zA-Z0-9",
    alpha: "a-zA-Z",
    ascii: "\\x00-\\x7F",
    blank: " \\t",
    cntrl: "\\x00-\\x1F\\x7F",
    digit: "0-9",
    graph: "\\x21-\\x7E",
    lower: "a-z",
    print: "\\x20-\\x7E ",
    punct: "\\-!\"#$%&'()\\*+,./:;<=>?@[\\]^_`{|}~",
    space: " \\t\\r\\n\\v\\f",
    upper: "A-Z",
    word: "A-Za-z0-9_",
    xdigit: "A-Fa-f0-9"
  };
  module.exports = {
    DEFAULT_MAX_EXTGLOB_RECURSION,
    MAX_LENGTH: 1024 * 64,
    POSIX_REGEX_SOURCE,
    REGEX_BACKSLASH: /\\(?![*+?^${}(|)[\]])/g,
    REGEX_NON_SPECIAL_CHARS: /^[^@![\].,$*+?^{}()|\\/]+/,
    REGEX_SPECIAL_CHARS: /[-*+?.^${}(|)[\]]/,
    REGEX_SPECIAL_CHARS_BACKREF: /(\\?)((\W)(\3*))/g,
    REGEX_SPECIAL_CHARS_GLOBAL: /([-*+?.^${}(|)[\]])/g,
    REGEX_REMOVE_BACKSLASH: /(?:\[.*?[^\\]\]|\\(?=.))/g,
    REPLACEMENTS: {
      __proto__: null,
      "***": "*",
      "**/**": "**",
      "**/**/**": "**"
    },
    CHAR_0: 48,
    CHAR_9: 57,
    CHAR_UPPERCASE_A: 65,
    CHAR_LOWERCASE_A: 97,
    CHAR_UPPERCASE_Z: 90,
    CHAR_LOWERCASE_Z: 122,
    CHAR_LEFT_PARENTHESES: 40,
    CHAR_RIGHT_PARENTHESES: 41,
    CHAR_ASTERISK: 42,
    CHAR_AMPERSAND: 38,
    CHAR_AT: 64,
    CHAR_BACKWARD_SLASH: 92,
    CHAR_CARRIAGE_RETURN: 13,
    CHAR_CIRCUMFLEX_ACCENT: 94,
    CHAR_COLON: 58,
    CHAR_COMMA: 44,
    CHAR_DOT: 46,
    CHAR_DOUBLE_QUOTE: 34,
    CHAR_EQUAL: 61,
    CHAR_EXCLAMATION_MARK: 33,
    CHAR_FORM_FEED: 12,
    CHAR_FORWARD_SLASH: 47,
    CHAR_GRAVE_ACCENT: 96,
    CHAR_HASH: 35,
    CHAR_HYPHEN_MINUS: 45,
    CHAR_LEFT_ANGLE_BRACKET: 60,
    CHAR_LEFT_CURLY_BRACE: 123,
    CHAR_LEFT_SQUARE_BRACKET: 91,
    CHAR_LINE_FEED: 10,
    CHAR_NO_BREAK_SPACE: 160,
    CHAR_PERCENT: 37,
    CHAR_PLUS: 43,
    CHAR_QUESTION_MARK: 63,
    CHAR_RIGHT_ANGLE_BRACKET: 62,
    CHAR_RIGHT_CURLY_BRACE: 125,
    CHAR_RIGHT_SQUARE_BRACKET: 93,
    CHAR_SEMICOLON: 59,
    CHAR_SINGLE_QUOTE: 39,
    CHAR_SPACE: 32,
    CHAR_TAB: 9,
    CHAR_UNDERSCORE: 95,
    CHAR_VERTICAL_LINE: 124,
    CHAR_ZERO_WIDTH_NOBREAK_SPACE: 65279,
    extglobChars(chars) {
      return {
        "!": { type: "negate", open: "(?:(?!(?:", close: `))${chars.STAR})` },
        "?": { type: "qmark", open: "(?:", close: ")?" },
        "+": { type: "plus", open: "(?:", close: ")+" },
        "*": { type: "star", open: "(?:", close: ")*" },
        "@": { type: "at", open: "(?:", close: ")" }
      };
    },
    globChars(win32) {
      return win32 === true ? WINDOWS_CHARS : POSIX_CHARS;
    }
  };
});

// node_modules/.bun/picomatch@4.0.4/node_modules/picomatch/lib/utils.js
var require_utils = __commonJS((exports) => {
  var {
    REGEX_BACKSLASH,
    REGEX_REMOVE_BACKSLASH,
    REGEX_SPECIAL_CHARS,
    REGEX_SPECIAL_CHARS_GLOBAL
  } = require_constants();
  exports.isObject = (val) => val !== null && typeof val === "object" && !Array.isArray(val);
  exports.hasRegexChars = (str) => REGEX_SPECIAL_CHARS.test(str);
  exports.isRegexChar = (str) => str.length === 1 && exports.hasRegexChars(str);
  exports.escapeRegex = (str) => str.replace(REGEX_SPECIAL_CHARS_GLOBAL, "\\$1");
  exports.toPosixSlashes = (str) => str.replace(REGEX_BACKSLASH, "/");
  exports.isWindows = () => {
    if (typeof navigator !== "undefined" && navigator.platform) {
      const platform = navigator.platform.toLowerCase();
      return platform === "win32" || platform === "windows";
    }
    if (typeof process !== "undefined" && process.platform) {
      return process.platform === "win32";
    }
    return false;
  };
  exports.removeBackslashes = (str) => {
    return str.replace(REGEX_REMOVE_BACKSLASH, (match) => {
      return match === "\\" ? "" : match;
    });
  };
  exports.escapeLast = (input, char, lastIdx) => {
    const idx = input.lastIndexOf(char, lastIdx);
    if (idx === -1)
      return input;
    if (input[idx - 1] === "\\")
      return exports.escapeLast(input, char, idx - 1);
    return `${input.slice(0, idx)}\\${input.slice(idx)}`;
  };
  exports.removePrefix = (input, state = {}) => {
    let output = input;
    if (output.startsWith("./")) {
      output = output.slice(2);
      state.prefix = "./";
    }
    return output;
  };
  exports.wrapOutput = (input, state = {}, options = {}) => {
    const prepend = options.contains ? "" : "^";
    const append = options.contains ? "" : "$";
    let output = `${prepend}(?:${input})${append}`;
    if (state.negated === true) {
      output = `(?:^(?!${output}).*$)`;
    }
    return output;
  };
  exports.basename = (path, { windows } = {}) => {
    const segs = path.split(windows ? /[\\/]/ : "/");
    const last = segs[segs.length - 1];
    if (last === "") {
      return segs[segs.length - 2];
    }
    return last;
  };
});

// node_modules/.bun/picomatch@4.0.4/node_modules/picomatch/lib/scan.js
var require_scan = __commonJS((exports, module) => {
  var utils = require_utils();
  var {
    CHAR_ASTERISK,
    CHAR_AT,
    CHAR_BACKWARD_SLASH,
    CHAR_COMMA,
    CHAR_DOT,
    CHAR_EXCLAMATION_MARK,
    CHAR_FORWARD_SLASH,
    CHAR_LEFT_CURLY_BRACE,
    CHAR_LEFT_PARENTHESES,
    CHAR_LEFT_SQUARE_BRACKET,
    CHAR_PLUS,
    CHAR_QUESTION_MARK,
    CHAR_RIGHT_CURLY_BRACE,
    CHAR_RIGHT_PARENTHESES,
    CHAR_RIGHT_SQUARE_BRACKET
  } = require_constants();
  var isPathSeparator = (code) => {
    return code === CHAR_FORWARD_SLASH || code === CHAR_BACKWARD_SLASH;
  };
  var depth = (token) => {
    if (token.isPrefix !== true) {
      token.depth = token.isGlobstar ? Infinity : 1;
    }
  };
  var scan = (input, options) => {
    const opts = options || {};
    const length = input.length - 1;
    const scanToEnd = opts.parts === true || opts.scanToEnd === true;
    const slashes = [];
    const tokens = [];
    const parts = [];
    let str = input;
    let index = -1;
    let start = 0;
    let lastIndex = 0;
    let isBrace = false;
    let isBracket = false;
    let isGlob = false;
    let isExtglob = false;
    let isGlobstar = false;
    let braceEscaped = false;
    let backslashes = false;
    let negated = false;
    let negatedExtglob = false;
    let finished = false;
    let braces = 0;
    let prev;
    let code;
    let token = { value: "", depth: 0, isGlob: false };
    const eos = () => index >= length;
    const peek = () => str.charCodeAt(index + 1);
    const advance = () => {
      prev = code;
      return str.charCodeAt(++index);
    };
    while (index < length) {
      code = advance();
      let next;
      if (code === CHAR_BACKWARD_SLASH) {
        backslashes = token.backslashes = true;
        code = advance();
        if (code === CHAR_LEFT_CURLY_BRACE) {
          braceEscaped = true;
        }
        continue;
      }
      if (braceEscaped === true || code === CHAR_LEFT_CURLY_BRACE) {
        braces++;
        while (eos() !== true && (code = advance())) {
          if (code === CHAR_BACKWARD_SLASH) {
            backslashes = token.backslashes = true;
            advance();
            continue;
          }
          if (code === CHAR_LEFT_CURLY_BRACE) {
            braces++;
            continue;
          }
          if (braceEscaped !== true && code === CHAR_DOT && (code = advance()) === CHAR_DOT) {
            isBrace = token.isBrace = true;
            isGlob = token.isGlob = true;
            finished = true;
            if (scanToEnd === true) {
              continue;
            }
            break;
          }
          if (braceEscaped !== true && code === CHAR_COMMA) {
            isBrace = token.isBrace = true;
            isGlob = token.isGlob = true;
            finished = true;
            if (scanToEnd === true) {
              continue;
            }
            break;
          }
          if (code === CHAR_RIGHT_CURLY_BRACE) {
            braces--;
            if (braces === 0) {
              braceEscaped = false;
              isBrace = token.isBrace = true;
              finished = true;
              break;
            }
          }
        }
        if (scanToEnd === true) {
          continue;
        }
        break;
      }
      if (code === CHAR_FORWARD_SLASH) {
        slashes.push(index);
        tokens.push(token);
        token = { value: "", depth: 0, isGlob: false };
        if (finished === true)
          continue;
        if (prev === CHAR_DOT && index === start + 1) {
          start += 2;
          continue;
        }
        lastIndex = index + 1;
        continue;
      }
      if (opts.noext !== true) {
        const isExtglobChar = code === CHAR_PLUS || code === CHAR_AT || code === CHAR_ASTERISK || code === CHAR_QUESTION_MARK || code === CHAR_EXCLAMATION_MARK;
        if (isExtglobChar === true && peek() === CHAR_LEFT_PARENTHESES) {
          isGlob = token.isGlob = true;
          isExtglob = token.isExtglob = true;
          finished = true;
          if (code === CHAR_EXCLAMATION_MARK && index === start) {
            negatedExtglob = true;
          }
          if (scanToEnd === true) {
            while (eos() !== true && (code = advance())) {
              if (code === CHAR_BACKWARD_SLASH) {
                backslashes = token.backslashes = true;
                code = advance();
                continue;
              }
              if (code === CHAR_RIGHT_PARENTHESES) {
                isGlob = token.isGlob = true;
                finished = true;
                break;
              }
            }
            continue;
          }
          break;
        }
      }
      if (code === CHAR_ASTERISK) {
        if (prev === CHAR_ASTERISK)
          isGlobstar = token.isGlobstar = true;
        isGlob = token.isGlob = true;
        finished = true;
        if (scanToEnd === true) {
          continue;
        }
        break;
      }
      if (code === CHAR_QUESTION_MARK) {
        isGlob = token.isGlob = true;
        finished = true;
        if (scanToEnd === true) {
          continue;
        }
        break;
      }
      if (code === CHAR_LEFT_SQUARE_BRACKET) {
        while (eos() !== true && (next = advance())) {
          if (next === CHAR_BACKWARD_SLASH) {
            backslashes = token.backslashes = true;
            advance();
            continue;
          }
          if (next === CHAR_RIGHT_SQUARE_BRACKET) {
            isBracket = token.isBracket = true;
            isGlob = token.isGlob = true;
            finished = true;
            break;
          }
        }
        if (scanToEnd === true) {
          continue;
        }
        break;
      }
      if (opts.nonegate !== true && code === CHAR_EXCLAMATION_MARK && index === start) {
        negated = token.negated = true;
        start++;
        continue;
      }
      if (opts.noparen !== true && code === CHAR_LEFT_PARENTHESES) {
        isGlob = token.isGlob = true;
        if (scanToEnd === true) {
          while (eos() !== true && (code = advance())) {
            if (code === CHAR_LEFT_PARENTHESES) {
              backslashes = token.backslashes = true;
              code = advance();
              continue;
            }
            if (code === CHAR_RIGHT_PARENTHESES) {
              finished = true;
              break;
            }
          }
          continue;
        }
        break;
      }
      if (isGlob === true) {
        finished = true;
        if (scanToEnd === true) {
          continue;
        }
        break;
      }
    }
    if (opts.noext === true) {
      isExtglob = false;
      isGlob = false;
    }
    let base = str;
    let prefix = "";
    let glob = "";
    if (start > 0) {
      prefix = str.slice(0, start);
      str = str.slice(start);
      lastIndex -= start;
    }
    if (base && isGlob === true && lastIndex > 0) {
      base = str.slice(0, lastIndex);
      glob = str.slice(lastIndex);
    } else if (isGlob === true) {
      base = "";
      glob = str;
    } else {
      base = str;
    }
    if (base && base !== "" && base !== "/" && base !== str) {
      if (isPathSeparator(base.charCodeAt(base.length - 1))) {
        base = base.slice(0, -1);
      }
    }
    if (opts.unescape === true) {
      if (glob)
        glob = utils.removeBackslashes(glob);
      if (base && backslashes === true) {
        base = utils.removeBackslashes(base);
      }
    }
    const state = {
      prefix,
      input,
      start,
      base,
      glob,
      isBrace,
      isBracket,
      isGlob,
      isExtglob,
      isGlobstar,
      negated,
      negatedExtglob
    };
    if (opts.tokens === true) {
      state.maxDepth = 0;
      if (!isPathSeparator(code)) {
        tokens.push(token);
      }
      state.tokens = tokens;
    }
    if (opts.parts === true || opts.tokens === true) {
      let prevIndex;
      for (let idx = 0;idx < slashes.length; idx++) {
        const n = prevIndex ? prevIndex + 1 : start;
        const i = slashes[idx];
        const value = input.slice(n, i);
        if (opts.tokens) {
          if (idx === 0 && start !== 0) {
            tokens[idx].isPrefix = true;
            tokens[idx].value = prefix;
          } else {
            tokens[idx].value = value;
          }
          depth(tokens[idx]);
          state.maxDepth += tokens[idx].depth;
        }
        if (idx !== 0 || value !== "") {
          parts.push(value);
        }
        prevIndex = i;
      }
      if (prevIndex && prevIndex + 1 < input.length) {
        const value = input.slice(prevIndex + 1);
        parts.push(value);
        if (opts.tokens) {
          tokens[tokens.length - 1].value = value;
          depth(tokens[tokens.length - 1]);
          state.maxDepth += tokens[tokens.length - 1].depth;
        }
      }
      state.slashes = slashes;
      state.parts = parts;
    }
    return state;
  };
  module.exports = scan;
});

// node_modules/.bun/picomatch@4.0.4/node_modules/picomatch/lib/parse.js
var require_parse = __commonJS((exports, module) => {
  var constants = require_constants();
  var utils = require_utils();
  var {
    MAX_LENGTH,
    POSIX_REGEX_SOURCE,
    REGEX_NON_SPECIAL_CHARS,
    REGEX_SPECIAL_CHARS_BACKREF,
    REPLACEMENTS
  } = constants;
  var expandRange = (args, options) => {
    if (typeof options.expandRange === "function") {
      return options.expandRange(...args, options);
    }
    args.sort();
    const value = `[${args.join("-")}]`;
    try {
      new RegExp(value);
    } catch (ex) {
      return args.map((v) => utils.escapeRegex(v)).join("..");
    }
    return value;
  };
  var syntaxError = (type, char) => {
    return `Missing ${type}: "${char}" - use "\\\\${char}" to match literal characters`;
  };
  var splitTopLevel = (input) => {
    const parts = [];
    let bracket = 0;
    let paren = 0;
    let quote = 0;
    let value = "";
    let escaped = false;
    for (const ch of input) {
      if (escaped === true) {
        value += ch;
        escaped = false;
        continue;
      }
      if (ch === "\\") {
        value += ch;
        escaped = true;
        continue;
      }
      if (ch === '"') {
        quote = quote === 1 ? 0 : 1;
        value += ch;
        continue;
      }
      if (quote === 0) {
        if (ch === "[") {
          bracket++;
        } else if (ch === "]" && bracket > 0) {
          bracket--;
        } else if (bracket === 0) {
          if (ch === "(") {
            paren++;
          } else if (ch === ")" && paren > 0) {
            paren--;
          } else if (ch === "|" && paren === 0) {
            parts.push(value);
            value = "";
            continue;
          }
        }
      }
      value += ch;
    }
    parts.push(value);
    return parts;
  };
  var isPlainBranch = (branch) => {
    let escaped = false;
    for (const ch of branch) {
      if (escaped === true) {
        escaped = false;
        continue;
      }
      if (ch === "\\") {
        escaped = true;
        continue;
      }
      if (/[?*+@!()[\]{}]/.test(ch)) {
        return false;
      }
    }
    return true;
  };
  var normalizeSimpleBranch = (branch) => {
    let value = branch.trim();
    let changed = true;
    while (changed === true) {
      changed = false;
      if (/^@\([^\\()[\]{}|]+\)$/.test(value)) {
        value = value.slice(2, -1);
        changed = true;
      }
    }
    if (!isPlainBranch(value)) {
      return;
    }
    return value.replace(/\\(.)/g, "$1");
  };
  var hasRepeatedCharPrefixOverlap = (branches) => {
    const values = branches.map(normalizeSimpleBranch).filter(Boolean);
    for (let i = 0;i < values.length; i++) {
      for (let j = i + 1;j < values.length; j++) {
        const a = values[i];
        const b = values[j];
        const char = a[0];
        if (!char || a !== char.repeat(a.length) || b !== char.repeat(b.length)) {
          continue;
        }
        if (a === b || a.startsWith(b) || b.startsWith(a)) {
          return true;
        }
      }
    }
    return false;
  };
  var parseRepeatedExtglob = (pattern, requireEnd = true) => {
    if (pattern[0] !== "+" && pattern[0] !== "*" || pattern[1] !== "(") {
      return;
    }
    let bracket = 0;
    let paren = 0;
    let quote = 0;
    let escaped = false;
    for (let i = 1;i < pattern.length; i++) {
      const ch = pattern[i];
      if (escaped === true) {
        escaped = false;
        continue;
      }
      if (ch === "\\") {
        escaped = true;
        continue;
      }
      if (ch === '"') {
        quote = quote === 1 ? 0 : 1;
        continue;
      }
      if (quote === 1) {
        continue;
      }
      if (ch === "[") {
        bracket++;
        continue;
      }
      if (ch === "]" && bracket > 0) {
        bracket--;
        continue;
      }
      if (bracket > 0) {
        continue;
      }
      if (ch === "(") {
        paren++;
        continue;
      }
      if (ch === ")") {
        paren--;
        if (paren === 0) {
          if (requireEnd === true && i !== pattern.length - 1) {
            return;
          }
          return {
            type: pattern[0],
            body: pattern.slice(2, i),
            end: i
          };
        }
      }
    }
  };
  var getStarExtglobSequenceOutput = (pattern) => {
    let index = 0;
    const chars = [];
    while (index < pattern.length) {
      const match = parseRepeatedExtglob(pattern.slice(index), false);
      if (!match || match.type !== "*") {
        return;
      }
      const branches = splitTopLevel(match.body).map((branch2) => branch2.trim());
      if (branches.length !== 1) {
        return;
      }
      const branch = normalizeSimpleBranch(branches[0]);
      if (!branch || branch.length !== 1) {
        return;
      }
      chars.push(branch);
      index += match.end + 1;
    }
    if (chars.length < 1) {
      return;
    }
    const source2 = chars.length === 1 ? utils.escapeRegex(chars[0]) : `[${chars.map((ch) => utils.escapeRegex(ch)).join("")}]`;
    return `${source2}*`;
  };
  var repeatedExtglobRecursion = (pattern) => {
    let depth = 0;
    let value = pattern.trim();
    let match = parseRepeatedExtglob(value);
    while (match) {
      depth++;
      value = match.body.trim();
      match = parseRepeatedExtglob(value);
    }
    return depth;
  };
  var analyzeRepeatedExtglob = (body, options) => {
    if (options.maxExtglobRecursion === false) {
      return { risky: false };
    }
    const max = typeof options.maxExtglobRecursion === "number" ? options.maxExtglobRecursion : constants.DEFAULT_MAX_EXTGLOB_RECURSION;
    const branches = splitTopLevel(body).map((branch) => branch.trim());
    if (branches.length > 1) {
      if (branches.some((branch) => branch === "") || branches.some((branch) => /^[*?]+$/.test(branch)) || hasRepeatedCharPrefixOverlap(branches)) {
        return { risky: true };
      }
    }
    for (const branch of branches) {
      const safeOutput = getStarExtglobSequenceOutput(branch);
      if (safeOutput) {
        return { risky: true, safeOutput };
      }
      if (repeatedExtglobRecursion(branch) > max) {
        return { risky: true };
      }
    }
    return { risky: false };
  };
  var parse3 = (input, options) => {
    if (typeof input !== "string") {
      throw new TypeError("Expected a string");
    }
    input = REPLACEMENTS[input] || input;
    const opts = { ...options };
    const max = typeof opts.maxLength === "number" ? Math.min(MAX_LENGTH, opts.maxLength) : MAX_LENGTH;
    let len = input.length;
    if (len > max) {
      throw new SyntaxError(`Input length: ${len}, exceeds maximum allowed length: ${max}`);
    }
    const bos = { type: "bos", value: "", output: opts.prepend || "" };
    const tokens = [bos];
    const capture = opts.capture ? "" : "?:";
    const PLATFORM_CHARS = constants.globChars(opts.windows);
    const EXTGLOB_CHARS = constants.extglobChars(PLATFORM_CHARS);
    const {
      DOT_LITERAL,
      PLUS_LITERAL,
      SLASH_LITERAL,
      ONE_CHAR,
      DOTS_SLASH,
      NO_DOT,
      NO_DOT_SLASH,
      NO_DOTS_SLASH,
      QMARK,
      QMARK_NO_DOT,
      STAR,
      START_ANCHOR
    } = PLATFORM_CHARS;
    const globstar = (opts2) => {
      return `(${capture}(?:(?!${START_ANCHOR}${opts2.dot ? DOTS_SLASH : DOT_LITERAL}).)*?)`;
    };
    const nodot = opts.dot ? "" : NO_DOT;
    const qmarkNoDot = opts.dot ? QMARK : QMARK_NO_DOT;
    let star = opts.bash === true ? globstar(opts) : STAR;
    if (opts.capture) {
      star = `(${star})`;
    }
    if (typeof opts.noext === "boolean") {
      opts.noextglob = opts.noext;
    }
    const state = {
      input,
      index: -1,
      start: 0,
      dot: opts.dot === true,
      consumed: "",
      output: "",
      prefix: "",
      backtrack: false,
      negated: false,
      brackets: 0,
      braces: 0,
      parens: 0,
      quotes: 0,
      globstar: false,
      tokens
    };
    input = utils.removePrefix(input, state);
    len = input.length;
    const extglobs = [];
    const braces = [];
    const stack = [];
    let prev = bos;
    let value;
    const eos = () => state.index === len - 1;
    const peek = state.peek = (n = 1) => input[state.index + n];
    const advance = state.advance = () => input[++state.index] || "";
    const remaining = () => input.slice(state.index + 1);
    const consume = (value2 = "", num = 0) => {
      state.consumed += value2;
      state.index += num;
    };
    const append = (token) => {
      state.output += token.output != null ? token.output : token.value;
      consume(token.value);
    };
    const negate = () => {
      let count = 1;
      while (peek() === "!" && (peek(2) !== "(" || peek(3) === "?")) {
        advance();
        state.start++;
        count++;
      }
      if (count % 2 === 0) {
        return false;
      }
      state.negated = true;
      state.start++;
      return true;
    };
    const increment = (type) => {
      state[type]++;
      stack.push(type);
    };
    const decrement = (type) => {
      state[type]--;
      stack.pop();
    };
    const push = (tok) => {
      if (prev.type === "globstar") {
        const isBrace = state.braces > 0 && (tok.type === "comma" || tok.type === "brace");
        const isExtglob = tok.extglob === true || extglobs.length && (tok.type === "pipe" || tok.type === "paren");
        if (tok.type !== "slash" && tok.type !== "paren" && !isBrace && !isExtglob) {
          state.output = state.output.slice(0, -prev.output.length);
          prev.type = "star";
          prev.value = "*";
          prev.output = star;
          state.output += prev.output;
        }
      }
      if (extglobs.length && tok.type !== "paren") {
        extglobs[extglobs.length - 1].inner += tok.value;
      }
      if (tok.value || tok.output)
        append(tok);
      if (prev && prev.type === "text" && tok.type === "text") {
        prev.output = (prev.output || prev.value) + tok.value;
        prev.value += tok.value;
        return;
      }
      tok.prev = prev;
      tokens.push(tok);
      prev = tok;
    };
    const extglobOpen = (type, value2) => {
      const token = { ...EXTGLOB_CHARS[value2], conditions: 1, inner: "" };
      token.prev = prev;
      token.parens = state.parens;
      token.output = state.output;
      token.startIndex = state.index;
      token.tokensIndex = tokens.length;
      const output = (opts.capture ? "(" : "") + token.open;
      increment("parens");
      push({ type, value: value2, output: state.output ? "" : ONE_CHAR });
      push({ type: "paren", extglob: true, value: advance(), output });
      extglobs.push(token);
    };
    const extglobClose = (token) => {
      const literal = input.slice(token.startIndex, state.index + 1);
      const body = input.slice(token.startIndex + 2, state.index);
      const analysis = analyzeRepeatedExtglob(body, opts);
      if ((token.type === "plus" || token.type === "star") && analysis.risky) {
        const safeOutput = analysis.safeOutput ? (token.output ? "" : ONE_CHAR) + (opts.capture ? `(${analysis.safeOutput})` : analysis.safeOutput) : undefined;
        const open = tokens[token.tokensIndex];
        open.type = "text";
        open.value = literal;
        open.output = safeOutput || utils.escapeRegex(literal);
        for (let i = token.tokensIndex + 1;i < tokens.length; i++) {
          tokens[i].value = "";
          tokens[i].output = "";
          delete tokens[i].suffix;
        }
        state.output = token.output + open.output;
        state.backtrack = true;
        push({ type: "paren", extglob: true, value, output: "" });
        decrement("parens");
        return;
      }
      let output = token.close + (opts.capture ? ")" : "");
      let rest;
      if (token.type === "negate") {
        let extglobStar = star;
        if (token.inner && token.inner.length > 1 && token.inner.includes("/")) {
          extglobStar = globstar(opts);
        }
        if (extglobStar !== star || eos() || /^\)+$/.test(remaining())) {
          output = token.close = `)$))${extglobStar}`;
        }
        if (token.inner.includes("*") && (rest = remaining()) && /^\.[^\\/.]+$/.test(rest)) {
          const expression = parse3(rest, { ...options, fastpaths: false }).output;
          output = token.close = `)${expression})${extglobStar})`;
        }
        if (token.prev.type === "bos") {
          state.negatedExtglob = true;
        }
      }
      push({ type: "paren", extglob: true, value, output });
      decrement("parens");
    };
    if (opts.fastpaths !== false && !/(^[*!]|[/()[\]{}"])/.test(input)) {
      let backslashes = false;
      let output = input.replace(REGEX_SPECIAL_CHARS_BACKREF, (m, esc, chars, first, rest, index) => {
        if (first === "\\") {
          backslashes = true;
          return m;
        }
        if (first === "?") {
          if (esc) {
            return esc + first + (rest ? QMARK.repeat(rest.length) : "");
          }
          if (index === 0) {
            return qmarkNoDot + (rest ? QMARK.repeat(rest.length) : "");
          }
          return QMARK.repeat(chars.length);
        }
        if (first === ".") {
          return DOT_LITERAL.repeat(chars.length);
        }
        if (first === "*") {
          if (esc) {
            return esc + first + (rest ? star : "");
          }
          return star;
        }
        return esc ? m : `\\${m}`;
      });
      if (backslashes === true) {
        if (opts.unescape === true) {
          output = output.replace(/\\/g, "");
        } else {
          output = output.replace(/\\+/g, (m) => {
            return m.length % 2 === 0 ? "\\\\" : m ? "\\" : "";
          });
        }
      }
      if (output === input && opts.contains === true) {
        state.output = input;
        return state;
      }
      state.output = utils.wrapOutput(output, state, options);
      return state;
    }
    while (!eos()) {
      value = advance();
      if (value === "\x00") {
        continue;
      }
      if (value === "\\") {
        const next = peek();
        if (next === "/" && opts.bash !== true) {
          continue;
        }
        if (next === "." || next === ";") {
          continue;
        }
        if (!next) {
          value += "\\";
          push({ type: "text", value });
          continue;
        }
        const match = /^\\+/.exec(remaining());
        let slashes = 0;
        if (match && match[0].length > 2) {
          slashes = match[0].length;
          state.index += slashes;
          if (slashes % 2 !== 0) {
            value += "\\";
          }
        }
        if (opts.unescape === true) {
          value = advance();
        } else {
          value += advance();
        }
        if (state.brackets === 0) {
          push({ type: "text", value });
          continue;
        }
      }
      if (state.brackets > 0 && (value !== "]" || prev.value === "[" || prev.value === "[^")) {
        if (opts.posix !== false && value === ":") {
          const inner = prev.value.slice(1);
          if (inner.includes("[")) {
            prev.posix = true;
            if (inner.includes(":")) {
              const idx = prev.value.lastIndexOf("[");
              const pre = prev.value.slice(0, idx);
              const rest2 = prev.value.slice(idx + 2);
              const posix = POSIX_REGEX_SOURCE[rest2];
              if (posix) {
                prev.value = pre + posix;
                state.backtrack = true;
                advance();
                if (!bos.output && tokens.indexOf(prev) === 1) {
                  bos.output = ONE_CHAR;
                }
                continue;
              }
            }
          }
        }
        if (value === "[" && peek() !== ":" || value === "-" && peek() === "]") {
          value = `\\${value}`;
        }
        if (value === "]" && (prev.value === "[" || prev.value === "[^")) {
          value = `\\${value}`;
        }
        if (opts.posix === true && value === "!" && prev.value === "[") {
          value = "^";
        }
        prev.value += value;
        append({ value });
        continue;
      }
      if (state.quotes === 1 && value !== '"') {
        value = utils.escapeRegex(value);
        prev.value += value;
        append({ value });
        continue;
      }
      if (value === '"') {
        state.quotes = state.quotes === 1 ? 0 : 1;
        if (opts.keepQuotes === true) {
          push({ type: "text", value });
        }
        continue;
      }
      if (value === "(") {
        increment("parens");
        push({ type: "paren", value });
        continue;
      }
      if (value === ")") {
        if (state.parens === 0 && opts.strictBrackets === true) {
          throw new SyntaxError(syntaxError("opening", "("));
        }
        const extglob = extglobs[extglobs.length - 1];
        if (extglob && state.parens === extglob.parens + 1) {
          extglobClose(extglobs.pop());
          continue;
        }
        push({ type: "paren", value, output: state.parens ? ")" : "\\)" });
        decrement("parens");
        continue;
      }
      if (value === "[") {
        if (opts.nobracket === true || !remaining().includes("]")) {
          if (opts.nobracket !== true && opts.strictBrackets === true) {
            throw new SyntaxError(syntaxError("closing", "]"));
          }
          value = `\\${value}`;
        } else {
          increment("brackets");
        }
        push({ type: "bracket", value });
        continue;
      }
      if (value === "]") {
        if (opts.nobracket === true || prev && prev.type === "bracket" && prev.value.length === 1) {
          push({ type: "text", value, output: `\\${value}` });
          continue;
        }
        if (state.brackets === 0) {
          if (opts.strictBrackets === true) {
            throw new SyntaxError(syntaxError("opening", "["));
          }
          push({ type: "text", value, output: `\\${value}` });
          continue;
        }
        decrement("brackets");
        const prevValue = prev.value.slice(1);
        if (prev.posix !== true && prevValue[0] === "^" && !prevValue.includes("/")) {
          value = `/${value}`;
        }
        prev.value += value;
        append({ value });
        if (opts.literalBrackets === false || utils.hasRegexChars(prevValue)) {
          continue;
        }
        const escaped = utils.escapeRegex(prev.value);
        state.output = state.output.slice(0, -prev.value.length);
        if (opts.literalBrackets === true) {
          state.output += escaped;
          prev.value = escaped;
          continue;
        }
        prev.value = `(${capture}${escaped}|${prev.value})`;
        state.output += prev.value;
        continue;
      }
      if (value === "{" && opts.nobrace !== true) {
        increment("braces");
        const open = {
          type: "brace",
          value,
          output: "(",
          outputIndex: state.output.length,
          tokensIndex: state.tokens.length
        };
        braces.push(open);
        push(open);
        continue;
      }
      if (value === "}") {
        const brace = braces[braces.length - 1];
        if (opts.nobrace === true || !brace) {
          push({ type: "text", value, output: value });
          continue;
        }
        let output = ")";
        if (brace.dots === true) {
          const arr = tokens.slice();
          const range = [];
          for (let i = arr.length - 1;i >= 0; i--) {
            tokens.pop();
            if (arr[i].type === "brace") {
              break;
            }
            if (arr[i].type !== "dots") {
              range.unshift(arr[i].value);
            }
          }
          output = expandRange(range, opts);
          state.backtrack = true;
        }
        if (brace.comma !== true && brace.dots !== true) {
          const out = state.output.slice(0, brace.outputIndex);
          const toks = state.tokens.slice(brace.tokensIndex);
          brace.value = brace.output = "\\{";
          value = output = "\\}";
          state.output = out;
          for (const t of toks) {
            state.output += t.output || t.value;
          }
        }
        push({ type: "brace", value, output });
        decrement("braces");
        braces.pop();
        continue;
      }
      if (value === "|") {
        if (extglobs.length > 0) {
          extglobs[extglobs.length - 1].conditions++;
        }
        push({ type: "text", value });
        continue;
      }
      if (value === ",") {
        let output = value;
        const brace = braces[braces.length - 1];
        if (brace && stack[stack.length - 1] === "braces") {
          brace.comma = true;
          output = "|";
        }
        push({ type: "comma", value, output });
        continue;
      }
      if (value === "/") {
        if (prev.type === "dot" && state.index === state.start + 1) {
          state.start = state.index + 1;
          state.consumed = "";
          state.output = "";
          tokens.pop();
          prev = bos;
          continue;
        }
        push({ type: "slash", value, output: SLASH_LITERAL });
        continue;
      }
      if (value === ".") {
        if (state.braces > 0 && prev.type === "dot") {
          if (prev.value === ".")
            prev.output = DOT_LITERAL;
          const brace = braces[braces.length - 1];
          prev.type = "dots";
          prev.output += value;
          prev.value += value;
          brace.dots = true;
          continue;
        }
        if (state.braces + state.parens === 0 && prev.type !== "bos" && prev.type !== "slash") {
          push({ type: "text", value, output: DOT_LITERAL });
          continue;
        }
        push({ type: "dot", value, output: DOT_LITERAL });
        continue;
      }
      if (value === "?") {
        const isGroup = prev && prev.value === "(";
        if (!isGroup && opts.noextglob !== true && peek() === "(" && peek(2) !== "?") {
          extglobOpen("qmark", value);
          continue;
        }
        if (prev && prev.type === "paren") {
          const next = peek();
          let output = value;
          if (prev.value === "(" && !/[!=<:]/.test(next) || next === "<" && !/<([!=]|\w+>)/.test(remaining())) {
            output = `\\${value}`;
          }
          push({ type: "text", value, output });
          continue;
        }
        if (opts.dot !== true && (prev.type === "slash" || prev.type === "bos")) {
          push({ type: "qmark", value, output: QMARK_NO_DOT });
          continue;
        }
        push({ type: "qmark", value, output: QMARK });
        continue;
      }
      if (value === "!") {
        if (opts.noextglob !== true && peek() === "(") {
          if (peek(2) !== "?" || !/[!=<:]/.test(peek(3))) {
            extglobOpen("negate", value);
            continue;
          }
        }
        if (opts.nonegate !== true && state.index === 0) {
          negate();
          continue;
        }
      }
      if (value === "+") {
        if (opts.noextglob !== true && peek() === "(" && peek(2) !== "?") {
          extglobOpen("plus", value);
          continue;
        }
        if (prev && prev.value === "(" || opts.regex === false) {
          push({ type: "plus", value, output: PLUS_LITERAL });
          continue;
        }
        if (prev && (prev.type === "bracket" || prev.type === "paren" || prev.type === "brace") || state.parens > 0) {
          push({ type: "plus", value });
          continue;
        }
        push({ type: "plus", value: PLUS_LITERAL });
        continue;
      }
      if (value === "@") {
        if (opts.noextglob !== true && peek() === "(" && peek(2) !== "?") {
          push({ type: "at", extglob: true, value, output: "" });
          continue;
        }
        push({ type: "text", value });
        continue;
      }
      if (value !== "*") {
        if (value === "$" || value === "^") {
          value = `\\${value}`;
        }
        const match = REGEX_NON_SPECIAL_CHARS.exec(remaining());
        if (match) {
          value += match[0];
          state.index += match[0].length;
        }
        push({ type: "text", value });
        continue;
      }
      if (prev && (prev.type === "globstar" || prev.star === true)) {
        prev.type = "star";
        prev.star = true;
        prev.value += value;
        prev.output = star;
        state.backtrack = true;
        state.globstar = true;
        consume(value);
        continue;
      }
      let rest = remaining();
      if (opts.noextglob !== true && /^\([^?]/.test(rest)) {
        extglobOpen("star", value);
        continue;
      }
      if (prev.type === "star") {
        if (opts.noglobstar === true) {
          consume(value);
          continue;
        }
        const prior = prev.prev;
        const before = prior.prev;
        const isStart = prior.type === "slash" || prior.type === "bos";
        const afterStar = before && (before.type === "star" || before.type === "globstar");
        if (opts.bash === true && (!isStart || rest[0] && rest[0] !== "/")) {
          push({ type: "star", value, output: "" });
          continue;
        }
        const isBrace = state.braces > 0 && (prior.type === "comma" || prior.type === "brace");
        const isExtglob = extglobs.length && (prior.type === "pipe" || prior.type === "paren");
        if (!isStart && prior.type !== "paren" && !isBrace && !isExtglob) {
          push({ type: "star", value, output: "" });
          continue;
        }
        while (rest.slice(0, 3) === "/**") {
          const after = input[state.index + 4];
          if (after && after !== "/") {
            break;
          }
          rest = rest.slice(3);
          consume("/**", 3);
        }
        if (prior.type === "bos" && eos()) {
          prev.type = "globstar";
          prev.value += value;
          prev.output = globstar(opts);
          state.output = prev.output;
          state.globstar = true;
          consume(value);
          continue;
        }
        if (prior.type === "slash" && prior.prev.type !== "bos" && !afterStar && eos()) {
          state.output = state.output.slice(0, -(prior.output + prev.output).length);
          prior.output = `(?:${prior.output}`;
          prev.type = "globstar";
          prev.output = globstar(opts) + (opts.strictSlashes ? ")" : "|$)");
          prev.value += value;
          state.globstar = true;
          state.output += prior.output + prev.output;
          consume(value);
          continue;
        }
        if (prior.type === "slash" && prior.prev.type !== "bos" && rest[0] === "/") {
          const end = rest[1] !== undefined ? "|$" : "";
          state.output = state.output.slice(0, -(prior.output + prev.output).length);
          prior.output = `(?:${prior.output}`;
          prev.type = "globstar";
          prev.output = `${globstar(opts)}${SLASH_LITERAL}|${SLASH_LITERAL}${end})`;
          prev.value += value;
          state.output += prior.output + prev.output;
          state.globstar = true;
          consume(value + advance());
          push({ type: "slash", value: "/", output: "" });
          continue;
        }
        if (prior.type === "bos" && rest[0] === "/") {
          prev.type = "globstar";
          prev.value += value;
          prev.output = `(?:^|${SLASH_LITERAL}|${globstar(opts)}${SLASH_LITERAL})`;
          state.output = prev.output;
          state.globstar = true;
          consume(value + advance());
          push({ type: "slash", value: "/", output: "" });
          continue;
        }
        state.output = state.output.slice(0, -prev.output.length);
        prev.type = "globstar";
        prev.output = globstar(opts);
        prev.value += value;
        state.output += prev.output;
        state.globstar = true;
        consume(value);
        continue;
      }
      const token = { type: "star", value, output: star };
      if (opts.bash === true) {
        token.output = ".*?";
        if (prev.type === "bos" || prev.type === "slash") {
          token.output = nodot + token.output;
        }
        push(token);
        continue;
      }
      if (prev && (prev.type === "bracket" || prev.type === "paren") && opts.regex === true) {
        token.output = value;
        push(token);
        continue;
      }
      if (state.index === state.start || prev.type === "slash" || prev.type === "dot") {
        if (prev.type === "dot") {
          state.output += NO_DOT_SLASH;
          prev.output += NO_DOT_SLASH;
        } else if (opts.dot === true) {
          state.output += NO_DOTS_SLASH;
          prev.output += NO_DOTS_SLASH;
        } else {
          state.output += nodot;
          prev.output += nodot;
        }
        if (peek() !== "*") {
          state.output += ONE_CHAR;
          prev.output += ONE_CHAR;
        }
      }
      push(token);
    }
    while (state.brackets > 0) {
      if (opts.strictBrackets === true)
        throw new SyntaxError(syntaxError("closing", "]"));
      state.output = utils.escapeLast(state.output, "[");
      decrement("brackets");
    }
    while (state.parens > 0) {
      if (opts.strictBrackets === true)
        throw new SyntaxError(syntaxError("closing", ")"));
      state.output = utils.escapeLast(state.output, "(");
      decrement("parens");
    }
    while (state.braces > 0) {
      if (opts.strictBrackets === true)
        throw new SyntaxError(syntaxError("closing", "}"));
      state.output = utils.escapeLast(state.output, "{");
      decrement("braces");
    }
    if (opts.strictSlashes !== true && (prev.type === "star" || prev.type === "bracket")) {
      push({ type: "maybe_slash", value: "", output: `${SLASH_LITERAL}?` });
    }
    if (state.backtrack === true) {
      state.output = "";
      for (const token of state.tokens) {
        state.output += token.output != null ? token.output : token.value;
        if (token.suffix) {
          state.output += token.suffix;
        }
      }
    }
    return state;
  };
  parse3.fastpaths = (input, options) => {
    const opts = { ...options };
    const max = typeof opts.maxLength === "number" ? Math.min(MAX_LENGTH, opts.maxLength) : MAX_LENGTH;
    const len = input.length;
    if (len > max) {
      throw new SyntaxError(`Input length: ${len}, exceeds maximum allowed length: ${max}`);
    }
    input = REPLACEMENTS[input] || input;
    const {
      DOT_LITERAL,
      SLASH_LITERAL,
      ONE_CHAR,
      DOTS_SLASH,
      NO_DOT,
      NO_DOTS,
      NO_DOTS_SLASH,
      STAR,
      START_ANCHOR
    } = constants.globChars(opts.windows);
    const nodot = opts.dot ? NO_DOTS : NO_DOT;
    const slashDot = opts.dot ? NO_DOTS_SLASH : NO_DOT;
    const capture = opts.capture ? "" : "?:";
    const state = { negated: false, prefix: "" };
    let star = opts.bash === true ? ".*?" : STAR;
    if (opts.capture) {
      star = `(${star})`;
    }
    const globstar = (opts2) => {
      if (opts2.noglobstar === true)
        return star;
      return `(${capture}(?:(?!${START_ANCHOR}${opts2.dot ? DOTS_SLASH : DOT_LITERAL}).)*?)`;
    };
    const create = (str) => {
      switch (str) {
        case "*":
          return `${nodot}${ONE_CHAR}${star}`;
        case ".*":
          return `${DOT_LITERAL}${ONE_CHAR}${star}`;
        case "*.*":
          return `${nodot}${star}${DOT_LITERAL}${ONE_CHAR}${star}`;
        case "*/*":
          return `${nodot}${star}${SLASH_LITERAL}${ONE_CHAR}${slashDot}${star}`;
        case "**":
          return nodot + globstar(opts);
        case "**/*":
          return `(?:${nodot}${globstar(opts)}${SLASH_LITERAL})?${slashDot}${ONE_CHAR}${star}`;
        case "**/*.*":
          return `(?:${nodot}${globstar(opts)}${SLASH_LITERAL})?${slashDot}${star}${DOT_LITERAL}${ONE_CHAR}${star}`;
        case "**/.*":
          return `(?:${nodot}${globstar(opts)}${SLASH_LITERAL})?${DOT_LITERAL}${ONE_CHAR}${star}`;
        default: {
          const match = /^(.*?)\.(\w+)$/.exec(str);
          if (!match)
            return;
          const source3 = create(match[1]);
          if (!source3)
            return;
          return source3 + DOT_LITERAL + match[2];
        }
      }
    };
    const output = utils.removePrefix(input, state);
    let source2 = create(output);
    if (source2 && opts.strictSlashes !== true) {
      source2 += `${SLASH_LITERAL}?`;
    }
    return source2;
  };
  module.exports = parse3;
});

// node_modules/.bun/picomatch@4.0.4/node_modules/picomatch/lib/picomatch.js
var require_picomatch = __commonJS((exports, module) => {
  var scan = require_scan();
  var parse3 = require_parse();
  var utils = require_utils();
  var constants = require_constants();
  var isObject = (val) => val && typeof val === "object" && !Array.isArray(val);
  var picomatch = (glob, options, returnState = false) => {
    if (Array.isArray(glob)) {
      const fns = glob.map((input) => picomatch(input, options, returnState));
      const arrayMatcher = (str) => {
        for (const isMatch of fns) {
          const state2 = isMatch(str);
          if (state2)
            return state2;
        }
        return false;
      };
      return arrayMatcher;
    }
    const isState = isObject(glob) && glob.tokens && glob.input;
    if (glob === "" || typeof glob !== "string" && !isState) {
      throw new TypeError("Expected pattern to be a non-empty string");
    }
    const opts = options || {};
    const posix = opts.windows;
    const regex = isState ? picomatch.compileRe(glob, options) : picomatch.makeRe(glob, options, false, true);
    const state = regex.state;
    delete regex.state;
    let isIgnored = () => false;
    if (opts.ignore) {
      const ignoreOpts = { ...options, ignore: null, onMatch: null, onResult: null };
      isIgnored = picomatch(opts.ignore, ignoreOpts, returnState);
    }
    const matcher = (input, returnObject = false) => {
      const { isMatch, match, output } = picomatch.test(input, regex, options, { glob, posix });
      const result = { glob, state, regex, posix, input, output, match, isMatch };
      if (typeof opts.onResult === "function") {
        opts.onResult(result);
      }
      if (isMatch === false) {
        result.isMatch = false;
        return returnObject ? result : false;
      }
      if (isIgnored(input)) {
        if (typeof opts.onIgnore === "function") {
          opts.onIgnore(result);
        }
        result.isMatch = false;
        return returnObject ? result : false;
      }
      if (typeof opts.onMatch === "function") {
        opts.onMatch(result);
      }
      return returnObject ? result : true;
    };
    if (returnState) {
      matcher.state = state;
    }
    return matcher;
  };
  picomatch.test = (input, regex, options, { glob, posix } = {}) => {
    if (typeof input !== "string") {
      throw new TypeError("Expected input to be a string");
    }
    if (input === "") {
      return { isMatch: false, output: "" };
    }
    const opts = options || {};
    const format2 = opts.format || (posix ? utils.toPosixSlashes : null);
    let match = input === glob;
    let output = match && format2 ? format2(input) : input;
    if (match === false) {
      output = format2 ? format2(input) : input;
      match = output === glob;
    }
    if (match === false || opts.capture === true) {
      if (opts.matchBase === true || opts.basename === true) {
        match = picomatch.matchBase(input, regex, options, posix);
      } else {
        match = regex.exec(output);
      }
    }
    return { isMatch: Boolean(match), match, output };
  };
  picomatch.matchBase = (input, glob, options) => {
    const regex = glob instanceof RegExp ? glob : picomatch.makeRe(glob, options);
    return regex.test(utils.basename(input));
  };
  picomatch.isMatch = (str, patterns, options) => picomatch(patterns, options)(str);
  picomatch.parse = (pattern, options) => {
    if (Array.isArray(pattern))
      return pattern.map((p) => picomatch.parse(p, options));
    return parse3(pattern, { ...options, fastpaths: false });
  };
  picomatch.scan = (input, options) => scan(input, options);
  picomatch.compileRe = (state, options, returnOutput = false, returnState = false) => {
    if (returnOutput === true) {
      return state.output;
    }
    const opts = options || {};
    const prepend = opts.contains ? "" : "^";
    const append = opts.contains ? "" : "$";
    let source2 = `${prepend}(?:${state.output})${append}`;
    if (state && state.negated === true) {
      source2 = `^(?!${source2}).*$`;
    }
    const regex = picomatch.toRegex(source2, options);
    if (returnState === true) {
      regex.state = state;
    }
    return regex;
  };
  picomatch.makeRe = (input, options = {}, returnOutput = false, returnState = false) => {
    if (!input || typeof input !== "string") {
      throw new TypeError("Expected a non-empty string");
    }
    let parsed = { negated: false, fastpaths: true };
    if (options.fastpaths !== false && (input[0] === "." || input[0] === "*")) {
      parsed.output = parse3.fastpaths(input, options);
    }
    if (!parsed.output) {
      parsed = parse3(input, options);
    }
    return picomatch.compileRe(parsed, options, returnOutput, returnState);
  };
  picomatch.toRegex = (source2, options) => {
    try {
      const opts = options || {};
      return new RegExp(source2, opts.flags || (opts.nocase ? "i" : ""));
    } catch (err) {
      if (options && options.debug === true)
        throw err;
      return /$^/;
    }
  };
  picomatch.constants = constants;
  module.exports = picomatch;
});

// node_modules/.bun/picomatch@4.0.4/node_modules/picomatch/index.js
var require_picomatch2 = __commonJS((exports, module) => {
  var pico = require_picomatch();
  var utils = require_utils();
  function picomatch(glob, options, returnState = false) {
    if (options && (options.windows === null || options.windows === undefined)) {
      options = { ...options, windows: utils.isWindows() };
    }
    return pico(glob, options, returnState);
  }
  Object.assign(picomatch, pico);
  module.exports = picomatch;
});

// sdk/typescript/src/internal/utf8.ts
var encoder = new TextEncoder;
var encode = TextEncoder.prototype.encode.call.bind(TextEncoder.prototype.encode);
var charCodeAt = String.prototype.charCodeAt.call.bind(String.prototype.charCodeAt);
function compareUTF8(left, right) {
  const leftBytes = encode(encoder, left);
  const rightBytes = encode(encoder, right);
  const length = leftBytes.length < rightBytes.length ? leftBytes.length : rightBytes.length;
  for (let index = 0;index < length; index++) {
    const difference = leftBytes[index] - rightBytes[index];
    if (difference !== 0)
      return difference;
  }
  return leftBytes.length - rightBytes.length;
}
function hasOnlyUnicodeScalarValues(value) {
  for (let index = 0;index < value.length; index++) {
    const unit = charCodeAt(value, index);
    if (unit >= 55296 && unit <= 56319) {
      if (index + 1 === value.length)
        return false;
      const next = charCodeAt(value, index + 1);
      if (next < 56320 || next > 57343)
        return false;
      index++;
    } else if (unit >= 56320 && unit <= 57343) {
      return false;
    }
  }
  return true;
}
function assertUnicodeString(value) {
  if (!hasOnlyUnicodeScalarValues(value)) {
    throw new Error("canonical JSON contains an unpaired surrogate");
  }
}

// sdk/typescript/src/config.ts
var arrayIsArray = Array.isArray;
var arrayPrototype = Array.prototype;
var defineProperty = Object.defineProperty;
var objectPrototype = Object.prototype;
var getOwnPropertyDescriptor = Object.getOwnPropertyDescriptor;
var getOwnPropertyDescriptors = Object.getOwnPropertyDescriptors;
var getPrototypeOf = Object.getPrototypeOf;
var hasOwn = Object.hasOwn;
var freeze = Object.freeze;
var ownKeys = Reflect.ownKeys;
var startsWith = String.prototype.startsWith.call.bind(String.prototype.startsWith);
var endsWith = String.prototype.endsWith.call.bind(String.prototype.endsWith);
var includes = String.prototype.includes.call.bind(String.prototype.includes);
var split = String.prototype.split.call.bind(String.prototype.split);
var slice = String.prototype.slice.call.bind(String.prototype.slice);
var charCodeAt2 = String.prototype.charCodeAt.call.bind(String.prototype.charCodeAt);
var regexpTest = RegExp.prototype.test.call.bind(RegExp.prototype.test);
function inspectConfig(value) {
  if (typeof value !== "object" || value === null) {
    throw new Error("config must be an ordinary object");
  }
  return normalizeConfig(value);
}
function matchesIgnorePattern(pattern, path) {
  const patternSegments = split(pattern, "/");
  const pathSegments = split(path, "/");
  const matches = (patternIndex, pathIndex) => {
    if (patternIndex === patternSegments.length) {
      return pathIndex === pathSegments.length;
    }
    const segment = patternSegments[patternIndex];
    if (segment === "**") {
      if (patternIndex === patternSegments.length - 1) {
        return pathIndex < pathSegments.length;
      }
      for (let candidate = pathIndex;candidate <= pathSegments.length; candidate++) {
        if (matches(patternIndex + 1, candidate))
          return true;
      }
      return false;
    }
    return pathIndex < pathSegments.length && matchesSegment(segment, pathSegments[pathIndex]) && matches(patternIndex + 1, pathIndex + 1);
  };
  return matches(0, 0);
}
function validateDirectory(value) {
  if (typeof value !== "string" || value === "" || !hasOnlyUnicodeScalarValues(value) || startsWith(value, "/") || includes(value, "\\") || hasControl(value)) {
    throw new Error("config dirs entries must be non-empty root-relative POSIX paths");
  }
  const normalized = startsWith(value, "./") ? slice(value, 2) : value;
  const segments = split(normalized, "/");
  let invalidSegment = normalized === "";
  for (let index = 0;index < segments.length; index++) {
    const segment = segments[index];
    if (segment === "" || segment === "." || segment === "..") {
      invalidSegment = true;
      break;
    }
  }
  if (invalidSegment) {
    throw new Error("config dirs entries must be normalized root-relative paths");
  }
  return normalized;
}
function validateIgnorePattern(value) {
  if (typeof value !== "string" || value === "" || !hasOnlyUnicodeScalarValues(value) || startsWith(value, "./") || startsWith(value, "/") || endsWith(value, "/") || includes(value, "//") || includes(value, "\\") || hasControl(value) || startsWith(value, "!") || regexpTest(/[[\]{}]/, value) || regexpTest(/[?*+@!]\(/, value)) {
    throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`);
  }
  const segments = split(value, "/");
  for (let index = 0;index < segments.length; index++) {
    const segment = segments[index];
    if (segment === ".." || includes(segment, "**") && segment !== "**") {
      throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`);
    }
  }
  return value;
}
function matchesSegment(pattern, value) {
  const patternCharacters = codePoints(pattern);
  const valueCharacters = codePoints(value);
  let patternIndex = 0;
  let valueIndex = 0;
  let star = -1;
  let starValue = -1;
  while (valueIndex < valueCharacters.length) {
    const token = patternCharacters[patternIndex];
    if (token === "?" || token === valueCharacters[valueIndex]) {
      patternIndex++;
      valueIndex++;
      continue;
    }
    if (token === "*") {
      star = patternIndex++;
      starValue = valueIndex;
      continue;
    }
    if (star !== -1) {
      patternIndex = star + 1;
      valueIndex = ++starValue;
      continue;
    }
    return false;
  }
  while (patternCharacters[patternIndex] === "*")
    patternIndex++;
  return patternIndex === patternCharacters.length;
}
function hasControl(value) {
  for (let index = 0;index < value.length; index++) {
    const code = charCodeAt2(value, index);
    if (code <= 31 || code >= 127 && code <= 159)
      return true;
  }
  return false;
}
function codePoints(value) {
  const result = [];
  for (let index = 0;index < value.length; ) {
    const first = charCodeAt2(value, index);
    const width = first >= 55296 && first <= 56319 ? 2 : 1;
    setArrayIndex(result, result.length, slice(value, index, index + width));
    index += width;
  }
  return result;
}
function normalizeConfig(value) {
  if (arrayIsArray(value) || getPrototypeOf(value) !== objectPrototype) {
    throw new Error("config must be an ordinary object");
  }
  const descriptors = getOwnPropertyDescriptors(value);
  const keys = ownKeys(value);
  let invalidKey = !hasOwn(descriptors, "dirs");
  for (let index = 0;index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string" || key !== "dirs" && key !== "ignorePatterns") {
      invalidKey = true;
      break;
    }
  }
  if (invalidKey) {
    throw new Error("config requires exactly dirs and optional ignorePatterns");
  }
  for (let index = 0;index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string") {
      throw new Error("config requires exactly dirs and optional ignorePatterns");
    }
    const descriptor = descriptors[key];
    if (descriptor === undefined || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error("config properties must be enumerable data properties");
    }
  }
  const dirs = normalizeStringSet(descriptors["dirs"]?.value, "config dirs", validateDirectory, true);
  const ignorePatterns = normalizeStringSet(hasOwn(descriptors, "ignorePatterns") ? descriptors["ignorePatterns"]?.value : [], "config ignorePatterns", validateIgnorePattern, false);
  return freeze({
    dirs: freeze(dirs),
    ignorePatterns: freeze(ignorePatterns)
  });
}
function normalizeStringSet(value, name, normalize, nonempty) {
  if (!arrayIsArray(value) || getPrototypeOf(value) !== arrayPrototype) {
    throw new Error(`${name} must be an array`);
  }
  const keys = ownKeys(value);
  const lengthDescriptor = getOwnPropertyDescriptor(value, "length");
  const length = lengthDescriptor?.value;
  if (typeof length !== "number" || keys.length !== length + 1 || keys[length] !== "length") {
    throw new Error(`${name} must be a dense ordinary array`);
  }
  const normalized = [];
  for (let index = 0;index < length; index++) {
    const key = `${index}`;
    if (keys[index] !== key) {
      throw new Error(`${name} must be a dense ordinary array`);
    }
    const descriptor = getOwnPropertyDescriptor(value, key);
    if (descriptor === undefined || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error(`${name} entries must be enumerable data properties`);
    }
    const current = normalize(descriptor.value);
    let insertion = normalized.length;
    while (insertion > 0 && compareUTF8(current, normalized[insertion - 1]) < 0) {
      setArrayIndex(normalized, insertion, normalized[insertion - 1]);
      insertion--;
    }
    setArrayIndex(normalized, insertion, current);
  }
  if (nonempty && length === 0) {
    throw new Error(`${name} must be non-empty`);
  }
  for (let index = 1;index < normalized.length; index++) {
    if (normalized[index] === normalized[index - 1]) {
      throw new Error(`${name} contains a duplicate entry`);
    }
  }
  return normalized;
}
function setArrayIndex(array, index, value) {
  defineProperty(array, `${index}`, {
    configurable: true,
    enumerable: true,
    value,
    writable: true
  });
}
// sdk/typescript/src/schema/payload.ts
var payloadSchemaValidationErrorBrand = Symbol.for("helmr.sdk.PayloadSchemaValidationError");
function assertPayloadSchema(value, label = "payload") {
  if (value === undefined) {
    return;
  }
  assertStandardSchema(value, label);
}
function assertStandardSchema(value, label = "schema") {
  if (value === null || typeof value !== "object" && typeof value !== "function") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`);
  }
  const standard = value["~standard"];
  if (standard === null || typeof standard !== "object") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`);
  }
  const record = standard;
  if (record["version"] !== 1 || typeof record["validate"] !== "function") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`);
  }
}

// sdk/typescript/src/schema/task.ts
var TASK_ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$";
var TASK_ID_MAX_LENGTH = 128;
var QUEUE_NAME_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$";
var QUEUE_NAME_MAX_LENGTH = 256;

class TaskIdError extends Error {
  name = "TaskIdError";
  value;
  constructor(value) {
    super(`task id must match ${TASK_ID_PATTERN}: ${JSON.stringify(value)}`);
    this.value = value;
  }
}
function validateTaskId(value) {
  if (!isValidTaskId(value)) {
    throw new TaskIdError(value);
  }
}
function isValidTaskId(value) {
  if (value.length === 0 || value.length > TASK_ID_MAX_LENGTH) {
    return false;
  }
  const first = value.charCodeAt(0);
  if (!isAsciiAlnum(first)) {
    return false;
  }
  for (let index = 1;index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (!(isAsciiAlnum(code) || code === 46 || code === 95 || code === 45)) {
      return false;
    }
  }
  return true;
}

class TaskQueueNameError extends Error {
  name = "TaskQueueNameError";
  value;
  constructor(value) {
    super(`queue name must match ${QUEUE_NAME_PATTERN}: ${JSON.stringify(value)}`);
    this.value = value;
  }
}

class TaskQueueConcurrencyLimitError extends Error {
  name = "TaskQueueConcurrencyLimitError";
  value;
  constructor(value) {
    super("queue concurrencyLimit must be a positive integer");
    this.value = value;
  }
}
function validateQueueName(value) {
  if (!isValidQueueName(value)) {
    throw new TaskQueueNameError(value);
  }
}
function isValidQueueName(value) {
  if (value.length === 0 || value.length > QUEUE_NAME_MAX_LENGTH) {
    return false;
  }
  const first = value.charCodeAt(0);
  if (!isAsciiAlnum(first)) {
    return false;
  }
  for (let index = 1;index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (!(isAsciiAlnum(code) || code === 46 || code === 95 || code === 45 || code === 47)) {
      return false;
    }
  }
  return true;
}
function validateOptionalQueueConcurrencyLimit(value) {
  if (value === undefined || value === null) {
    return;
  }
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) {
    return;
  }
  throw new TaskQueueConcurrencyLimitError(value);
}
function isAsciiAlnum(code) {
  return code >= 48 && code <= 57 || code >= 65 && code <= 90 || code >= 97 && code <= 122;
}

// sdk/typescript/src/internal/runtime.ts
var runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations");
function currentRuntimeOperations() {
  const operations = globalThis[runtimeOperationsSymbol];
  if (operations === undefined) {
    throw new Error("runtime operation is unavailable without the Helmr managed runtime");
  }
  return operations;
}

// sdk/typescript/src/internal/id.ts
var uuidV7Pattern = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
function resourceID(value, label) {
  if (typeof value !== "string" || !uuidV7Pattern.test(value)) {
    throw new Error(`${label} must be a canonical UUIDv7`);
  }
  return value;
}

// sdk/typescript/src/session.ts
function createRuntimeSessionRef(id) {
  const sessionID = resourceID(id, "Session ID");
  return Object.freeze({
    id: sessionID,
    input: Object.freeze({
      send(input, request, options) {
        return currentRuntimeOperations().actorInputSend(sessionID, input, request, options?.signal);
      }
    }),
    output: Object.freeze({
      list(query, options) {
        return currentRuntimeOperations().sessionOutputPage(sessionID, query, options?.signal);
      }
    }),
    retrieve(options = {}) {
      return currentRuntimeOperations().sessionRetrieve(sessionID, options.signal);
    },
    close(request, options) {
      return currentRuntimeOperations().sessionClose(sessionID, request, options?.signal);
    }
  });
}
var sessions = Object.freeze({
  ref(id) {
    return createRuntimeSessionRef(id);
  }
});

// sdk/typescript/src/definitions.ts
var privateDefinitionBrand = Symbol.for("helmr.sdk.v0.definition");
var privateQueueBrand = Symbol.for("helmr.sdk.v0.queue");
function inspectDefinition(value) {
  if (typeof value !== "object" && typeof value !== "function" || value === null) {
    return;
  }
  if (!Object.hasOwn(value, privateDefinitionBrand))
    return;
  const definition = value[privateDefinitionBrand];
  if (!isInternalDefinition(definition)) {
    throw new Error("invalid private definition record");
  }
  return definition;
}
function isQueue(value) {
  if (typeof value !== "object" || value === null)
    return false;
  if (!Object.hasOwn(value, privateQueueBrand))
    return false;
  if (value[privateQueueBrand] !== true) {
    throw new Error("invalid private queue record");
  }
  const queue = value;
  if (typeof queue.name !== "string") {
    throw new Error("invalid private queue record");
  }
  validateQueueName(queue.name);
  validateOptionalQueueConcurrencyLimit(queue.concurrencyLimit);
  return true;
}
function isInternalDefinition(value) {
  if (typeof value !== "object" || value === null)
    return false;
  const definition = value;
  if (typeof definition.id !== "string")
    return false;
  validateTaskId(definition.id);
  switch (definition.kind) {
    case "task":
      if (typeof definition.handler !== "function" || typeof definition.hasPayload !== "boolean") {
        return false;
      }
      if (definition.hasPayload) {
        assertPayloadSchema(definition.payloadSchema, `task ${JSON.stringify(definition.id)} payload`);
      } else if (Object.hasOwn(definition, "payloadSchema")) {
        return false;
      }
      return true;
    case "actor":
      return typeof definition.handler === "function";
    default:
      return false;
  }
}
// sdk/typescript/src/image.ts
var imageBrand = Symbol.for("helmr.sdk.v0.image");
var sourceFileBrand = Symbol.for("helmr.sdk.v0.source-file");
var sourceDirectoryBrand = Symbol.for("helmr.sdk.v0.source-directory");
class SourceFileValue {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceFileBrand, { value: true });
    Object.freeze(this);
  }
}

class SourceDirectoryValue {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceDirectoryBrand, { value: true });
    Object.freeze(this);
  }
}
var source = Object.freeze({
  file(path) {
    return new SourceFileValue(path);
  },
  directory(path) {
    return new SourceDirectoryValue(path);
  }
});
function inspectImage(value) {
  if (typeof value !== "object" || value === null || value[imageBrand] !== true) {
    return;
  }
  const imageValue = value;
  return { key: imageValue.key, steps: imageValue.steps };
}
// sdk/typescript/src/secret.ts
var secretAddressBrand = Symbol.for("helmr.sdk.v0.secret-address");
var secretNamePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;

class SecretNameAddress {
  name;
  constructor(name) {
    validateSecretName(name);
    this.name = name;
    Object.defineProperty(this, secretAddressBrand, { value: true });
    Object.freeze(this);
  }
}
var secrets = Object.freeze({
  fromName(name) {
    return new SecretNameAddress(name);
  }
});
function validateSecretName(value) {
  if (!secretNamePattern.test(value)) {
    throw new Error("Secret name is invalid");
  }
}

// sdk/typescript/src/workspace.ts
var sandboxDefinitionBrand = Symbol.for("helmr.sdk.v0.sandbox");
var workspaceAddressBrand = Symbol.for("helmr.sdk.v0.workspace-address");
var workspaces = Object.freeze({
  ref: createWorkspaceRef
});
function inspectSandboxDefinition(value) {
  if (typeof value !== "object" || value === null)
    return;
  if (!Object.hasOwn(value, sandboxDefinitionBrand))
    return;
  if (value[sandboxDefinitionBrand] !== true) {
    throw new Error("invalid private Sandbox record");
  }
  const internal = value.internal;
  if (typeof internal !== "object" || internal === null || internal.kind !== "sandbox" || typeof internal.id !== "string" || typeof internal.image !== "object" || internal.image === null || typeof internal.resources !== "object" || internal.resources === null) {
    throw new Error("invalid private Sandbox record");
  }
  validateTaskId(internal.id);
  return internal;
}
function createWorkspaceRef(id) {
  const workspaceID = resourceID(id, "Workspace ID");
  const files = Object.freeze({
    read(path, options) {
      return currentRuntimeOperations().workspaceFileRead(workspaceID, path, options?.signal);
    },
    stat(path, options) {
      return currentRuntimeOperations().workspaceFileStat(workspaceID, path, options?.signal);
    },
    list(path, query, options) {
      return currentRuntimeOperations().workspaceFileList(workspaceID, path, query, options?.signal);
    }
  });
  const operations = {
    files,
    retrieve(options) {
      return currentRuntimeOperations().workspaceRetrieve(workspaceID, options?.signal);
    },
    exec(request, options) {
      return currentRuntimeOperations().workspaceExec(workspaceID, request, options?.signal);
    },
    delete(request, options) {
      return currentRuntimeOperations().workspaceDelete(workspaceID, request, options?.signal);
    }
  };
  return brandWorkspaceAddress({ id: workspaceID, ...operations });
}
function brandWorkspaceAddress(value) {
  resourceID(value.id, "Workspace ID");
  return freezeWorkspaceAddress(value);
}
function freezeWorkspaceAddress(value) {
  Object.defineProperty(value, workspaceAddressBrand, { value: true });
  return Object.freeze(value);
}
// sdk/typescript/src/internal/jsoncanon.ts
var textEncoder = new TextEncoder;
function canonicalizeJsonValue(value) {
  return textEncoder.encode(serialize(value, new Set));
}
function serialize(value, ancestors) {
  if (value === null || typeof value === "boolean") {
    return String(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error("canonical JSON numbers must be finite IEEE 754 doubles");
    }
    return JSON.stringify(value);
  }
  if (typeof value === "string") {
    assertUnicodeString(value);
    return JSON.stringify(value);
  }
  if (typeof value !== "object") {
    throw new Error(`canonical JSON does not support ${typeof value}`);
  }
  if (ancestors.has(value)) {
    throw new Error("canonical JSON does not support cyclic values");
  }
  ancestors.add(value);
  try {
    if (Array.isArray(value)) {
      assertPlainArray(value);
      const items = value.map((item) => serialize(item, ancestors));
      return `[${items.join(",")}]`;
    }
    const objectValue = value;
    assertPlainObject(objectValue);
    const entries = Object.keys(objectValue).sort().map((key) => {
      assertUnicodeString(key);
      return `${JSON.stringify(key)}:${serialize(objectValue[key], ancestors)}`;
    });
    return `{${entries.join(",")}}`;
  } finally {
    ancestors.delete(value);
  }
}
function assertPlainArray(value) {
  const keys = Reflect.ownKeys(value);
  const expected = Array.from({ length: value.length }, (_, index) => String(index));
  expected.push("length");
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error("canonical JSON arrays must be dense and have no extra properties");
  }
}
function assertPlainObject(value) {
  const prototype = Object.getPrototypeOf(value);
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error("canonical JSON objects must have a plain or null prototype");
  }
  for (const key of Reflect.ownKeys(value)) {
    if (typeof key !== "string") {
      throw new Error("canonical JSON objects cannot have symbol properties");
    }
    const descriptor = Object.getOwnPropertyDescriptor(value, key);
    if (!descriptor?.enumerable || !("value" in descriptor)) {
      throw new Error("canonical JSON object properties must be enumerable data properties");
    }
  }
}
// compiler/typescript/src/program-compiler.ts
import { createWriteStream } from "node:fs";
import { mkdir as mkdir2, readFile as readFile3, writeFile as writeFile2 } from "node:fs/promises";
import { dirname as dirname4, resolve as resolve6 } from "node:path";

// compiler/typescript/src/analysis.ts
import { lstat, readdir, realpath } from "node:fs/promises";
import { relative, resolve, sep } from "node:path";

// compiler/typescript/src/utf8.ts
function compareUTF82(left, right) {
  return Buffer.compare(Buffer.from(left), Buffer.from(right));
}
function hasOnlyUnicodeScalarValues2(value) {
  for (let index = 0;index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 55296 && unit <= 56319) {
      if (index + 1 === value.length)
        return false;
      const next = value.charCodeAt(index + 1);
      if (next < 56320 || next > 57343)
        return false;
      index++;
    } else if (unit >= 56320 && unit <= 57343) {
      return false;
    }
  }
  return true;
}

// compiler/typescript/src/analysis.ts
var executableExtension = /\.(?:cjs|cts|js|jsx|mjs|mts|ts|tsx)$/;
var textDecoder = new TextDecoder("utf-8", { fatal: true });
var maxVerificationFailureMessageBytes = 16 << 10;
var VERIFICATION_RESULT_FORMAT_VERSION = 0;
function successfulVerificationResult(analysis) {
  const files = [{
    path: "helmr/build-plan.json",
    content: decodeGeneratedFile(analysis.buildPlanBytes)
  }];
  if (analysis.programDeclarations.length > 0) {
    files.push({
      path: "helmr/analysis-locators.json",
      content: decodeGeneratedFile(analysis.declarationLocatorBytes)
    }, {
      path: "helmr/entry.mjs",
      content: decodeGeneratedFile(analysis.entrypointBytes)
    });
  }
  return Object.freeze({
    formatVersion: VERIFICATION_RESULT_FORMAT_VERSION,
    outcome: "succeeded",
    declarations: analysis.programDeclarations,
    files: Object.freeze(files.map((file) => Object.freeze(file)))
  });
}
function failedVerificationResult(message) {
  const encoded = new TextEncoder().encode(message);
  if (encoded.length === 0 || encoded.length > maxVerificationFailureMessageBytes || message.trim() === "") {
    throw new Error(`verification failure message must be nonblank UTF-8 of at most ${maxVerificationFailureMessageBytes} bytes`);
  }
  return Object.freeze({
    formatVersion: VERIFICATION_RESULT_FORMAT_VERSION,
    outcome: "failed",
    error: Object.freeze({
      reason: "verification_failed",
      message
    })
  });
}
function encodeVerificationResultFrame(result) {
  const body = canonicalizeJsonValue(result);
  const frame = new Uint8Array(4 + body.length);
  new DataView(frame.buffer).setUint32(0, body.length, false);
  frame.set(body, 4);
  return frame;
}
async function discoverModules(root, config) {
  const canonicalRoot = await realpath(root);
  await rejectReservedRoot(canonicalRoot);
  const candidates = new Set;
  for (const configured of config.dirs) {
    const directory = resolve(canonicalRoot, configured);
    if (!inside(canonicalRoot, directory)) {
      throw new Error(`configured dir escapes the project root: ${configured}`);
    }
    const relativeDirectory = projectPath(canonicalRoot, directory);
    if (hasComponent(relativeDirectory, "node_modules")) {
      throw new Error(`configured dir enters the dependency namespace: ${configured}`);
    }
    if (hasComponent(relativeDirectory, ".helmr")) {
      throw new Error(`configured dir enters reserved Platform output: ${configured}`);
    }
    await requireUnlinkedDirectory(canonicalRoot, directory, configured);
    await appendCandidates(canonicalRoot, directory, candidates);
  }
  const modules = [...candidates].filter((path) => !config.ignorePatterns.some((pattern) => matchesIgnorePattern(pattern, path)));
  modules.sort(compareUTF82);
  return modules;
}
async function appendCandidates(root, directory, candidates) {
  const entries = await readdir(directory, { withFileTypes: true });
  entries.sort((left, right) => compareUTF82(left.name, right.name));
  for (const entry of entries) {
    const absolute = resolve(directory, entry.name);
    const path = projectPath(root, absolute);
    if (hasComponent(path, "node_modules"))
      continue;
    if (entry.name === ".helmr") {
      throw new Error(`declaration tree contains reserved Platform output: ${path}`);
    }
    const metadata = await lstat(absolute);
    if (metadata.isSymbolicLink())
      continue;
    if (metadata.isDirectory()) {
      await appendCandidates(root, absolute, candidates);
      continue;
    }
    if (metadata.isFile() && executableExtension.test(path) && !isDeclarationOnly(path) && path !== "helmr.config.ts") {
      candidates.add(path);
      continue;
    }
    if (!metadata.isFile()) {
      throw new Error(`unsupported declaration tree entry: ${path}`);
    }
  }
}
function isDeclarationOnly(path) {
  return path.endsWith(".d.ts") || path.endsWith(".d.mts") || path.endsWith(".d.cts");
}
async function rejectReservedRoot(root) {
  try {
    await lstat(resolve(root, "helmr"));
  } catch (error) {
    if (error.code === "ENOENT")
      return;
    throw error;
  }
  throw new Error("project root helmr/ is reserved for Platform output");
}
async function requireUnlinkedDirectory(root, directory, configured) {
  let metadata;
  try {
    metadata = await lstat(directory);
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new Error(`configured dir does not exist: ${configured}`);
    }
    throw error;
  }
  if (!metadata.isDirectory()) {
    throw new Error(`configured dir is not a regular directory: ${configured}`);
  }
  if (await realpath(directory) !== directory || !inside(root, directory)) {
    throw new Error(`configured dir traverses a symbolic link: ${configured}`);
  }
}
function projectPath(root, value) {
  return relative(root, value).split(sep).join("/");
}
function inside(root, value) {
  const path = relative(root, value);
  return path === "" || !path.startsWith(`..${sep}`) && path !== ".." && !path.startsWith("/");
}
function hasComponent(path, component) {
  return path.split("/").includes(component);
}
function decodeGeneratedFile(value) {
  try {
    return textDecoder.decode(value);
  } catch {
    throw new Error("generated analysis file is not valid UTF-8");
  }
}

// compiler/typescript/src/bundle.ts
import {
  build as build2,
  version as esbuildVersion
} from "esbuild";
import { createHash } from "node:crypto";
import { createRequire as createRequire2 } from "node:module";
import {
  mkdir,
  readFile as readFile2,
  realpath as realpath4,
  rm,
  stat as stat3,
  writeFile
} from "node:fs/promises";
import { dirname as dirname3, relative as relative4, resolve as resolve5, sep as sep4 } from "node:path";
import { pathToFileURL } from "node:url";

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/impl/scanner.js
function createScanner(text, ignoreTrivia = false) {
  const len = text.length;
  let pos = 0, value = "", tokenOffset = 0, token = 16, lineNumber = 0, lineStartOffset = 0, tokenLineStartOffset = 0, prevTokenLineStartOffset = 0, scanError = 0;
  function scanHexDigits(count, exact) {
    let digits = 0;
    let value2 = 0;
    while (digits < count || !exact) {
      let ch = text.charCodeAt(pos);
      if (ch >= 48 && ch <= 57) {
        value2 = value2 * 16 + ch - 48;
      } else if (ch >= 65 && ch <= 70) {
        value2 = value2 * 16 + ch - 65 + 10;
      } else if (ch >= 97 && ch <= 102) {
        value2 = value2 * 16 + ch - 97 + 10;
      } else {
        break;
      }
      pos++;
      digits++;
    }
    if (digits < count) {
      value2 = -1;
    }
    return value2;
  }
  function setPosition(newPosition) {
    pos = newPosition;
    value = "";
    tokenOffset = 0;
    token = 16;
    scanError = 0;
  }
  function scanNumber() {
    let start = pos;
    if (text.charCodeAt(pos) === 48) {
      pos++;
    } else {
      pos++;
      while (pos < text.length && isDigit(text.charCodeAt(pos))) {
        pos++;
      }
    }
    if (pos < text.length && text.charCodeAt(pos) === 46) {
      pos++;
      if (pos < text.length && isDigit(text.charCodeAt(pos))) {
        pos++;
        while (pos < text.length && isDigit(text.charCodeAt(pos))) {
          pos++;
        }
      } else {
        scanError = 3;
        return text.substring(start, pos);
      }
    }
    let end = pos;
    if (pos < text.length && (text.charCodeAt(pos) === 69 || text.charCodeAt(pos) === 101)) {
      pos++;
      if (pos < text.length && text.charCodeAt(pos) === 43 || text.charCodeAt(pos) === 45) {
        pos++;
      }
      if (pos < text.length && isDigit(text.charCodeAt(pos))) {
        pos++;
        while (pos < text.length && isDigit(text.charCodeAt(pos))) {
          pos++;
        }
        end = pos;
      } else {
        scanError = 3;
      }
    }
    return text.substring(start, end);
  }
  function scanString() {
    let result = "", start = pos;
    while (true) {
      if (pos >= len) {
        result += text.substring(start, pos);
        scanError = 2;
        break;
      }
      const ch = text.charCodeAt(pos);
      if (ch === 34) {
        result += text.substring(start, pos);
        pos++;
        break;
      }
      if (ch === 92) {
        result += text.substring(start, pos);
        pos++;
        if (pos >= len) {
          scanError = 2;
          break;
        }
        const ch2 = text.charCodeAt(pos++);
        switch (ch2) {
          case 34:
            result += '"';
            break;
          case 92:
            result += "\\";
            break;
          case 47:
            result += "/";
            break;
          case 98:
            result += "\b";
            break;
          case 102:
            result += "\f";
            break;
          case 110:
            result += `
`;
            break;
          case 114:
            result += "\r";
            break;
          case 116:
            result += "\t";
            break;
          case 117:
            const ch3 = scanHexDigits(4, true);
            if (ch3 >= 0) {
              result += String.fromCharCode(ch3);
            } else {
              scanError = 4;
            }
            break;
          default:
            scanError = 5;
        }
        start = pos;
        continue;
      }
      if (ch >= 0 && ch <= 31) {
        if (isLineBreak(ch)) {
          result += text.substring(start, pos);
          scanError = 2;
          break;
        } else {
          scanError = 6;
        }
      }
      pos++;
    }
    return result;
  }
  function scanNext() {
    value = "";
    scanError = 0;
    tokenOffset = pos;
    lineStartOffset = lineNumber;
    prevTokenLineStartOffset = tokenLineStartOffset;
    if (pos >= len) {
      tokenOffset = len;
      return token = 17;
    }
    let code = text.charCodeAt(pos);
    if (isWhiteSpace(code)) {
      do {
        pos++;
        value += String.fromCharCode(code);
        code = text.charCodeAt(pos);
      } while (isWhiteSpace(code));
      return token = 15;
    }
    if (isLineBreak(code)) {
      pos++;
      value += String.fromCharCode(code);
      if (code === 13 && text.charCodeAt(pos) === 10) {
        pos++;
        value += `
`;
      }
      lineNumber++;
      tokenLineStartOffset = pos;
      return token = 14;
    }
    switch (code) {
      case 123:
        pos++;
        return token = 1;
      case 125:
        pos++;
        return token = 2;
      case 91:
        pos++;
        return token = 3;
      case 93:
        pos++;
        return token = 4;
      case 58:
        pos++;
        return token = 6;
      case 44:
        pos++;
        return token = 5;
      case 34:
        pos++;
        value = scanString();
        return token = 10;
      case 47:
        const start = pos - 1;
        if (text.charCodeAt(pos + 1) === 47) {
          pos += 2;
          while (pos < len) {
            if (isLineBreak(text.charCodeAt(pos))) {
              break;
            }
            pos++;
          }
          value = text.substring(start, pos);
          return token = 12;
        }
        if (text.charCodeAt(pos + 1) === 42) {
          pos += 2;
          const safeLength = len - 1;
          let commentClosed = false;
          while (pos < safeLength) {
            const ch = text.charCodeAt(pos);
            if (ch === 42 && text.charCodeAt(pos + 1) === 47) {
              pos += 2;
              commentClosed = true;
              break;
            }
            pos++;
            if (isLineBreak(ch)) {
              if (ch === 13 && text.charCodeAt(pos) === 10) {
                pos++;
              }
              lineNumber++;
              tokenLineStartOffset = pos;
            }
          }
          if (!commentClosed) {
            pos++;
            scanError = 1;
          }
          value = text.substring(start, pos);
          return token = 13;
        }
        value += String.fromCharCode(code);
        pos++;
        return token = 16;
      case 45:
        value += String.fromCharCode(code);
        pos++;
        if (pos === len || !isDigit(text.charCodeAt(pos))) {
          return token = 16;
        }
      case 48:
      case 49:
      case 50:
      case 51:
      case 52:
      case 53:
      case 54:
      case 55:
      case 56:
      case 57:
        value += scanNumber();
        return token = 11;
      default:
        while (pos < len && isUnknownContentCharacter(code)) {
          pos++;
          code = text.charCodeAt(pos);
        }
        if (tokenOffset !== pos) {
          value = text.substring(tokenOffset, pos);
          switch (value) {
            case "true":
              return token = 8;
            case "false":
              return token = 9;
            case "null":
              return token = 7;
          }
          return token = 16;
        }
        value += String.fromCharCode(code);
        pos++;
        return token = 16;
    }
  }
  function isUnknownContentCharacter(code) {
    if (isWhiteSpace(code) || isLineBreak(code)) {
      return false;
    }
    switch (code) {
      case 125:
      case 93:
      case 123:
      case 91:
      case 34:
      case 58:
      case 44:
      case 47:
        return false;
    }
    return true;
  }
  function scanNextNonTrivia() {
    let result;
    do {
      result = scanNext();
    } while (result >= 12 && result <= 15);
    return result;
  }
  return {
    setPosition,
    getPosition: () => pos,
    scan: ignoreTrivia ? scanNextNonTrivia : scanNext,
    getToken: () => token,
    getTokenValue: () => value,
    getTokenOffset: () => tokenOffset,
    getTokenLength: () => pos - tokenOffset,
    getTokenStartLine: () => lineStartOffset,
    getTokenStartCharacter: () => tokenOffset - prevTokenLineStartOffset,
    getTokenError: () => scanError
  };
}
function isWhiteSpace(ch) {
  return ch === 32 || ch === 9;
}
function isLineBreak(ch) {
  return ch === 10 || ch === 13;
}
function isDigit(ch) {
  return ch >= 48 && ch <= 57;
}
var CharacterCodes;
(function(CharacterCodes2) {
  CharacterCodes2[CharacterCodes2["lineFeed"] = 10] = "lineFeed";
  CharacterCodes2[CharacterCodes2["carriageReturn"] = 13] = "carriageReturn";
  CharacterCodes2[CharacterCodes2["space"] = 32] = "space";
  CharacterCodes2[CharacterCodes2["_0"] = 48] = "_0";
  CharacterCodes2[CharacterCodes2["_1"] = 49] = "_1";
  CharacterCodes2[CharacterCodes2["_2"] = 50] = "_2";
  CharacterCodes2[CharacterCodes2["_3"] = 51] = "_3";
  CharacterCodes2[CharacterCodes2["_4"] = 52] = "_4";
  CharacterCodes2[CharacterCodes2["_5"] = 53] = "_5";
  CharacterCodes2[CharacterCodes2["_6"] = 54] = "_6";
  CharacterCodes2[CharacterCodes2["_7"] = 55] = "_7";
  CharacterCodes2[CharacterCodes2["_8"] = 56] = "_8";
  CharacterCodes2[CharacterCodes2["_9"] = 57] = "_9";
  CharacterCodes2[CharacterCodes2["a"] = 97] = "a";
  CharacterCodes2[CharacterCodes2["b"] = 98] = "b";
  CharacterCodes2[CharacterCodes2["c"] = 99] = "c";
  CharacterCodes2[CharacterCodes2["d"] = 100] = "d";
  CharacterCodes2[CharacterCodes2["e"] = 101] = "e";
  CharacterCodes2[CharacterCodes2["f"] = 102] = "f";
  CharacterCodes2[CharacterCodes2["g"] = 103] = "g";
  CharacterCodes2[CharacterCodes2["h"] = 104] = "h";
  CharacterCodes2[CharacterCodes2["i"] = 105] = "i";
  CharacterCodes2[CharacterCodes2["j"] = 106] = "j";
  CharacterCodes2[CharacterCodes2["k"] = 107] = "k";
  CharacterCodes2[CharacterCodes2["l"] = 108] = "l";
  CharacterCodes2[CharacterCodes2["m"] = 109] = "m";
  CharacterCodes2[CharacterCodes2["n"] = 110] = "n";
  CharacterCodes2[CharacterCodes2["o"] = 111] = "o";
  CharacterCodes2[CharacterCodes2["p"] = 112] = "p";
  CharacterCodes2[CharacterCodes2["q"] = 113] = "q";
  CharacterCodes2[CharacterCodes2["r"] = 114] = "r";
  CharacterCodes2[CharacterCodes2["s"] = 115] = "s";
  CharacterCodes2[CharacterCodes2["t"] = 116] = "t";
  CharacterCodes2[CharacterCodes2["u"] = 117] = "u";
  CharacterCodes2[CharacterCodes2["v"] = 118] = "v";
  CharacterCodes2[CharacterCodes2["w"] = 119] = "w";
  CharacterCodes2[CharacterCodes2["x"] = 120] = "x";
  CharacterCodes2[CharacterCodes2["y"] = 121] = "y";
  CharacterCodes2[CharacterCodes2["z"] = 122] = "z";
  CharacterCodes2[CharacterCodes2["A"] = 65] = "A";
  CharacterCodes2[CharacterCodes2["B"] = 66] = "B";
  CharacterCodes2[CharacterCodes2["C"] = 67] = "C";
  CharacterCodes2[CharacterCodes2["D"] = 68] = "D";
  CharacterCodes2[CharacterCodes2["E"] = 69] = "E";
  CharacterCodes2[CharacterCodes2["F"] = 70] = "F";
  CharacterCodes2[CharacterCodes2["G"] = 71] = "G";
  CharacterCodes2[CharacterCodes2["H"] = 72] = "H";
  CharacterCodes2[CharacterCodes2["I"] = 73] = "I";
  CharacterCodes2[CharacterCodes2["J"] = 74] = "J";
  CharacterCodes2[CharacterCodes2["K"] = 75] = "K";
  CharacterCodes2[CharacterCodes2["L"] = 76] = "L";
  CharacterCodes2[CharacterCodes2["M"] = 77] = "M";
  CharacterCodes2[CharacterCodes2["N"] = 78] = "N";
  CharacterCodes2[CharacterCodes2["O"] = 79] = "O";
  CharacterCodes2[CharacterCodes2["P"] = 80] = "P";
  CharacterCodes2[CharacterCodes2["Q"] = 81] = "Q";
  CharacterCodes2[CharacterCodes2["R"] = 82] = "R";
  CharacterCodes2[CharacterCodes2["S"] = 83] = "S";
  CharacterCodes2[CharacterCodes2["T"] = 84] = "T";
  CharacterCodes2[CharacterCodes2["U"] = 85] = "U";
  CharacterCodes2[CharacterCodes2["V"] = 86] = "V";
  CharacterCodes2[CharacterCodes2["W"] = 87] = "W";
  CharacterCodes2[CharacterCodes2["X"] = 88] = "X";
  CharacterCodes2[CharacterCodes2["Y"] = 89] = "Y";
  CharacterCodes2[CharacterCodes2["Z"] = 90] = "Z";
  CharacterCodes2[CharacterCodes2["asterisk"] = 42] = "asterisk";
  CharacterCodes2[CharacterCodes2["backslash"] = 92] = "backslash";
  CharacterCodes2[CharacterCodes2["closeBrace"] = 125] = "closeBrace";
  CharacterCodes2[CharacterCodes2["closeBracket"] = 93] = "closeBracket";
  CharacterCodes2[CharacterCodes2["colon"] = 58] = "colon";
  CharacterCodes2[CharacterCodes2["comma"] = 44] = "comma";
  CharacterCodes2[CharacterCodes2["dot"] = 46] = "dot";
  CharacterCodes2[CharacterCodes2["doubleQuote"] = 34] = "doubleQuote";
  CharacterCodes2[CharacterCodes2["minus"] = 45] = "minus";
  CharacterCodes2[CharacterCodes2["openBrace"] = 123] = "openBrace";
  CharacterCodes2[CharacterCodes2["openBracket"] = 91] = "openBracket";
  CharacterCodes2[CharacterCodes2["plus"] = 43] = "plus";
  CharacterCodes2[CharacterCodes2["slash"] = 47] = "slash";
  CharacterCodes2[CharacterCodes2["formFeed"] = 12] = "formFeed";
  CharacterCodes2[CharacterCodes2["tab"] = 9] = "tab";
})(CharacterCodes || (CharacterCodes = {}));

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/impl/string-intern.js
var cachedSpaces = new Array(20).fill(0).map((_, index) => {
  return " ".repeat(index);
});
var maxCachedValues = 200;
var cachedBreakLinesWithSpaces = {
  " ": {
    "\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `
` + " ".repeat(index);
    }),
    "\r": new Array(maxCachedValues).fill(0).map((_, index) => {
      return "\r" + " ".repeat(index);
    }),
    "\r\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `\r
` + " ".repeat(index);
    })
  },
  "\t": {
    "\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `
` + "\t".repeat(index);
    }),
    "\r": new Array(maxCachedValues).fill(0).map((_, index) => {
      return "\r" + "\t".repeat(index);
    }),
    "\r\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `\r
` + "\t".repeat(index);
    })
  }
};

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/impl/parser.js
var ParseOptions;
(function(ParseOptions2) {
  ParseOptions2.DEFAULT = {
    allowTrailingComma: false
  };
})(ParseOptions || (ParseOptions = {}));
function parse(text, errors = [], options = ParseOptions.DEFAULT) {
  let currentProperty = null;
  let currentParent = [];
  const previousParents = [];
  function onValue(value) {
    if (Array.isArray(currentParent)) {
      currentParent.push(value);
    } else if (currentProperty !== null) {
      currentParent[currentProperty] = value;
    }
  }
  const visitor = {
    onObjectBegin: () => {
      const object = {};
      onValue(object);
      previousParents.push(currentParent);
      currentParent = object;
      currentProperty = null;
    },
    onObjectProperty: (name) => {
      currentProperty = name;
    },
    onObjectEnd: () => {
      currentParent = previousParents.pop();
    },
    onArrayBegin: () => {
      const array = [];
      onValue(array);
      previousParents.push(currentParent);
      currentParent = array;
      currentProperty = null;
    },
    onArrayEnd: () => {
      currentParent = previousParents.pop();
    },
    onLiteralValue: onValue,
    onError: (error, offset, length) => {
      errors.push({ error, offset, length });
    }
  };
  visit(text, visitor, options);
  return currentParent[0];
}
function visit(text, visitor, options = ParseOptions.DEFAULT) {
  const _scanner = createScanner(text, false);
  const _jsonPath = [];
  let suppressedCallbacks = 0;
  function toNoArgVisit(visitFunction) {
    return visitFunction ? () => suppressedCallbacks === 0 && visitFunction(_scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter()) : () => true;
  }
  function toOneArgVisit(visitFunction) {
    return visitFunction ? (arg) => suppressedCallbacks === 0 && visitFunction(arg, _scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter()) : () => true;
  }
  function toOneArgVisitWithPath(visitFunction) {
    return visitFunction ? (arg) => suppressedCallbacks === 0 && visitFunction(arg, _scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter(), () => _jsonPath.slice()) : () => true;
  }
  function toBeginVisit(visitFunction) {
    return visitFunction ? () => {
      if (suppressedCallbacks > 0) {
        suppressedCallbacks++;
      } else {
        let cbReturn = visitFunction(_scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter(), () => _jsonPath.slice());
        if (cbReturn === false) {
          suppressedCallbacks = 1;
        }
      }
    } : () => true;
  }
  function toEndVisit(visitFunction) {
    return visitFunction ? () => {
      if (suppressedCallbacks > 0) {
        suppressedCallbacks--;
      }
      if (suppressedCallbacks === 0) {
        visitFunction(_scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter());
      }
    } : () => true;
  }
  const onObjectBegin = toBeginVisit(visitor.onObjectBegin), onObjectProperty = toOneArgVisitWithPath(visitor.onObjectProperty), onObjectEnd = toEndVisit(visitor.onObjectEnd), onArrayBegin = toBeginVisit(visitor.onArrayBegin), onArrayEnd = toEndVisit(visitor.onArrayEnd), onLiteralValue = toOneArgVisitWithPath(visitor.onLiteralValue), onSeparator = toOneArgVisit(visitor.onSeparator), onComment = toNoArgVisit(visitor.onComment), onError = toOneArgVisit(visitor.onError);
  const disallowComments = options && options.disallowComments;
  const allowTrailingComma = options && options.allowTrailingComma;
  function scanNext() {
    while (true) {
      const token = _scanner.scan();
      switch (_scanner.getTokenError()) {
        case 4:
          handleError(14);
          break;
        case 5:
          handleError(15);
          break;
        case 3:
          handleError(13);
          break;
        case 1:
          if (!disallowComments) {
            handleError(11);
          }
          break;
        case 2:
          handleError(12);
          break;
        case 6:
          handleError(16);
          break;
      }
      switch (token) {
        case 12:
        case 13:
          if (disallowComments) {
            handleError(10);
          } else {
            onComment();
          }
          break;
        case 16:
          handleError(1);
          break;
        case 15:
        case 14:
          break;
        default:
          return token;
      }
    }
  }
  function handleError(error, skipUntilAfter = [], skipUntil = []) {
    onError(error);
    if (skipUntilAfter.length + skipUntil.length > 0) {
      let token = _scanner.getToken();
      while (token !== 17) {
        if (skipUntilAfter.indexOf(token) !== -1) {
          scanNext();
          break;
        } else if (skipUntil.indexOf(token) !== -1) {
          break;
        }
        token = scanNext();
      }
    }
  }
  function parseString(isValue) {
    const value = _scanner.getTokenValue();
    if (isValue) {
      onLiteralValue(value);
    } else {
      onObjectProperty(value);
      _jsonPath.push(value);
    }
    scanNext();
    return true;
  }
  function parseLiteral() {
    switch (_scanner.getToken()) {
      case 11:
        const tokenValue = _scanner.getTokenValue();
        let value = Number(tokenValue);
        if (isNaN(value)) {
          handleError(2);
          value = 0;
        }
        onLiteralValue(value);
        break;
      case 7:
        onLiteralValue(null);
        break;
      case 8:
        onLiteralValue(true);
        break;
      case 9:
        onLiteralValue(false);
        break;
      default:
        return false;
    }
    scanNext();
    return true;
  }
  function parseProperty() {
    if (_scanner.getToken() !== 10) {
      handleError(3, [], [2, 5]);
      return false;
    }
    parseString(false);
    if (_scanner.getToken() === 6) {
      onSeparator(":");
      scanNext();
      if (!parseValue()) {
        handleError(4, [], [2, 5]);
      }
    } else {
      handleError(5, [], [2, 5]);
    }
    _jsonPath.pop();
    return true;
  }
  function parseObject() {
    onObjectBegin();
    scanNext();
    let needsComma = false;
    while (_scanner.getToken() !== 2 && _scanner.getToken() !== 17) {
      if (_scanner.getToken() === 5) {
        if (!needsComma) {
          handleError(4, [], []);
        }
        onSeparator(",");
        scanNext();
        if (_scanner.getToken() === 2 && allowTrailingComma) {
          break;
        }
      } else if (needsComma) {
        handleError(6, [], []);
      }
      if (!parseProperty()) {
        handleError(4, [], [2, 5]);
      }
      needsComma = true;
    }
    onObjectEnd();
    if (_scanner.getToken() !== 2) {
      handleError(7, [2], []);
    } else {
      scanNext();
    }
    return true;
  }
  function parseArray() {
    onArrayBegin();
    scanNext();
    let isFirstElement = true;
    let needsComma = false;
    while (_scanner.getToken() !== 4 && _scanner.getToken() !== 17) {
      if (_scanner.getToken() === 5) {
        if (!needsComma) {
          handleError(4, [], []);
        }
        onSeparator(",");
        scanNext();
        if (_scanner.getToken() === 4 && allowTrailingComma) {
          break;
        }
      } else if (needsComma) {
        handleError(6, [], []);
      }
      if (isFirstElement) {
        _jsonPath.push(0);
        isFirstElement = false;
      } else {
        _jsonPath[_jsonPath.length - 1]++;
      }
      if (!parseValue()) {
        handleError(4, [], [4, 5]);
      }
      needsComma = true;
    }
    onArrayEnd();
    if (!isFirstElement) {
      _jsonPath.pop();
    }
    if (_scanner.getToken() !== 4) {
      handleError(8, [4], []);
    } else {
      scanNext();
    }
    return true;
  }
  function parseValue() {
    switch (_scanner.getToken()) {
      case 3:
        return parseArray();
      case 1:
        return parseObject();
      case 10:
        return parseString(true);
      default:
        return parseLiteral();
    }
  }
  scanNext();
  if (_scanner.getToken() === 17) {
    if (options.allowEmptyContent) {
      return true;
    }
    handleError(4, [], []);
    return false;
  }
  if (!parseValue()) {
    handleError(4, [], []);
    return false;
  }
  if (_scanner.getToken() !== 17) {
    handleError(9, [], []);
  }
  return true;
}

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/main.js
var ScanError;
(function(ScanError2) {
  ScanError2[ScanError2["None"] = 0] = "None";
  ScanError2[ScanError2["UnexpectedEndOfComment"] = 1] = "UnexpectedEndOfComment";
  ScanError2[ScanError2["UnexpectedEndOfString"] = 2] = "UnexpectedEndOfString";
  ScanError2[ScanError2["UnexpectedEndOfNumber"] = 3] = "UnexpectedEndOfNumber";
  ScanError2[ScanError2["InvalidUnicode"] = 4] = "InvalidUnicode";
  ScanError2[ScanError2["InvalidEscapeCharacter"] = 5] = "InvalidEscapeCharacter";
  ScanError2[ScanError2["InvalidCharacter"] = 6] = "InvalidCharacter";
})(ScanError || (ScanError = {}));
var SyntaxKind;
(function(SyntaxKind2) {
  SyntaxKind2[SyntaxKind2["OpenBraceToken"] = 1] = "OpenBraceToken";
  SyntaxKind2[SyntaxKind2["CloseBraceToken"] = 2] = "CloseBraceToken";
  SyntaxKind2[SyntaxKind2["OpenBracketToken"] = 3] = "OpenBracketToken";
  SyntaxKind2[SyntaxKind2["CloseBracketToken"] = 4] = "CloseBracketToken";
  SyntaxKind2[SyntaxKind2["CommaToken"] = 5] = "CommaToken";
  SyntaxKind2[SyntaxKind2["ColonToken"] = 6] = "ColonToken";
  SyntaxKind2[SyntaxKind2["NullKeyword"] = 7] = "NullKeyword";
  SyntaxKind2[SyntaxKind2["TrueKeyword"] = 8] = "TrueKeyword";
  SyntaxKind2[SyntaxKind2["FalseKeyword"] = 9] = "FalseKeyword";
  SyntaxKind2[SyntaxKind2["StringLiteral"] = 10] = "StringLiteral";
  SyntaxKind2[SyntaxKind2["NumericLiteral"] = 11] = "NumericLiteral";
  SyntaxKind2[SyntaxKind2["LineCommentTrivia"] = 12] = "LineCommentTrivia";
  SyntaxKind2[SyntaxKind2["BlockCommentTrivia"] = 13] = "BlockCommentTrivia";
  SyntaxKind2[SyntaxKind2["LineBreakTrivia"] = 14] = "LineBreakTrivia";
  SyntaxKind2[SyntaxKind2["Trivia"] = 15] = "Trivia";
  SyntaxKind2[SyntaxKind2["Unknown"] = 16] = "Unknown";
  SyntaxKind2[SyntaxKind2["EOF"] = 17] = "EOF";
})(SyntaxKind || (SyntaxKind = {}));
var parse2 = parse;
var ParseErrorCode;
(function(ParseErrorCode2) {
  ParseErrorCode2[ParseErrorCode2["InvalidSymbol"] = 1] = "InvalidSymbol";
  ParseErrorCode2[ParseErrorCode2["InvalidNumberFormat"] = 2] = "InvalidNumberFormat";
  ParseErrorCode2[ParseErrorCode2["PropertyNameExpected"] = 3] = "PropertyNameExpected";
  ParseErrorCode2[ParseErrorCode2["ValueExpected"] = 4] = "ValueExpected";
  ParseErrorCode2[ParseErrorCode2["ColonExpected"] = 5] = "ColonExpected";
  ParseErrorCode2[ParseErrorCode2["CommaExpected"] = 6] = "CommaExpected";
  ParseErrorCode2[ParseErrorCode2["CloseBraceExpected"] = 7] = "CloseBraceExpected";
  ParseErrorCode2[ParseErrorCode2["CloseBracketExpected"] = 8] = "CloseBracketExpected";
  ParseErrorCode2[ParseErrorCode2["EndOfFileExpected"] = 9] = "EndOfFileExpected";
  ParseErrorCode2[ParseErrorCode2["InvalidCommentToken"] = 10] = "InvalidCommentToken";
  ParseErrorCode2[ParseErrorCode2["UnexpectedEndOfComment"] = 11] = "UnexpectedEndOfComment";
  ParseErrorCode2[ParseErrorCode2["UnexpectedEndOfString"] = 12] = "UnexpectedEndOfString";
  ParseErrorCode2[ParseErrorCode2["UnexpectedEndOfNumber"] = 13] = "UnexpectedEndOfNumber";
  ParseErrorCode2[ParseErrorCode2["InvalidUnicode"] = 14] = "InvalidUnicode";
  ParseErrorCode2[ParseErrorCode2["InvalidEscapeCharacter"] = 15] = "InvalidEscapeCharacter";
  ParseErrorCode2[ParseErrorCode2["InvalidCharacter"] = 16] = "InvalidCharacter";
})(ParseErrorCode || (ParseErrorCode = {}));

// compiler/typescript/src/compile.ts
var BUILD_PLAN_FORMAT_VERSION = 0;
var DECLARATION_LOCATOR_FORMAT_VERSION = 0;
var PROGRAM_ENTRYPOINT = `import { runProgram } from "file:///opt/helmr/runtime/helmr/entry.mjs";
await runProgram(new URL("./declarations.json", import.meta.url));
`;
function analyze(options) {
  const located = locateDefinitions(options);
  const queues = compileQueues(located, options.exports);
  const sandboxExports = new Map;
  for (const item of located) {
    if (item.definition.kind === "sandbox") {
      sandboxExports.set(item.definition.id, item.value);
    }
  }
  const definitions = located.map(({ definition }) => compileDefinition(definition, options, queues, sandboxExports));
  const programExports = compileProgramExports(located);
  const buildPlan = Object.freeze({
    formatVersion: BUILD_PLAN_FORMAT_VERSION,
    definitions: Object.freeze(definitions),
    queues: Object.freeze([...queues.values()].map((entry) => Object.freeze({ ...entry })).sort((left, right) => compareUTF82(left.name, right.name)))
  });
  const declarationLocator = Object.freeze({
    declarations: programExports.declarationLocator.declarations,
    formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION
  });
  return {
    buildPlan,
    buildPlanBytes: canonicalizeJsonValue(buildPlan),
    declarationLocator,
    declarationLocatorBytes: canonicalizeJsonValue(declarationLocator),
    programDeclarations: programExports.programDeclarations,
    entrypointBytes: new TextEncoder().encode(PROGRAM_ENTRYPOINT)
  };
}
function analyzeProgramExports(options) {
  return compileProgramExports(locateDefinitions(options));
}
function locateDefinitions(options) {
  if (options.architecture !== "x86_64") {
    throw new Error(`unsupported architecture ${JSON.stringify(options.architecture)}`);
  }
  const located = discoverDefinitions(options.exports);
  if (located.length === 0) {
    throw new Error("BuildPlan definitions must be non-empty");
  }
  if (located.length > 1e4) {
    throw new Error("BuildPlan definitions exceed 10000");
  }
  located.sort(compareLocatedDefinitions);
  return located;
}
function compileProgramExports(located) {
  const declarations = located.flatMap((item) => item.definition.kind === "sandbox" ? [] : [locatorEntry(item)]);
  const programDeclarations = located.flatMap(({ definition }) => definition.kind === "sandbox" ? [] : [programDeclaration(definition)]);
  return Object.freeze({
    declarationLocator: Object.freeze({
      declarations: Object.freeze(declarations),
      formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION
    }),
    programDeclarations: Object.freeze(programDeclarations)
  });
}
function normalizeWorkspaceResources(resources) {
  return Object.freeze({
    milliCpu: normalizeCpu(resources.cpu),
    memoryMiB: normalizeIecMiB(resources.memory, "memory")
  });
}
function discoverDefinitions(exports) {
  const identities = new Map;
  for (const item of exports) {
    const definition = inspectDefinition(item.value) ?? inspectSandboxDefinition(item.value);
    if (definition === undefined)
      continue;
    validateModulePath(item.modulePath);
    validateExportName(item.exportName);
    const key = `${definition.kind}\x00${definition.id}`;
    const existing = identities.get(key);
    if (existing !== undefined) {
      if (existing.value === item.value) {
        const candidate = {
          definition,
          modulePath: item.modulePath,
          exportName: item.exportName,
          value: item.value
        };
        if (compareLocatorOccurrence(candidate, existing.located) < 0) {
          existing.located = candidate;
        }
        continue;
      }
      throw new Error(`duplicate ${definition.kind} declaration ${JSON.stringify(definition.id)} at ${existing.located.modulePath}#${existing.located.exportName} and ${item.modulePath}#${item.exportName}`);
    }
    const located = {
      definition,
      modulePath: item.modulePath,
      exportName: item.exportName,
      value: item.value
    };
    identities.set(key, {
      value: item.value,
      located
    });
  }
  return [...identities.values()].map((item) => item.located);
}
function compileDefinition(definition, options, queues, sandboxExports) {
  switch (definition.kind) {
    case "task":
      return {
        kind: "task",
        declaredId: definition.id,
        manifest: {
          payload: {
            kind: definition.hasPayload ? "standard_schema" : "none"
          },
          run: normalizeRun(definition, "task", queues),
          ...definition.schedule === undefined ? {} : {
            schedule: compileSchedule(definition, sandboxExports)
          }
        }
      };
    case "actor":
      return {
        kind: "actor",
        declaredId: definition.id,
        manifest: {
          run: normalizeRun(definition, "actor", queues),
          idleTimeoutMs: definition.idleTimeout === undefined ? 30000 : normalizeDuration(definition.idleTimeout, `actor ${JSON.stringify(definition.id)} idleTimeout`, 1, 3600000)
        }
      };
    case "sandbox":
      return {
        kind: "sandbox",
        declaredId: definition.id,
        manifest: {
          imageBuild: compileImageBuild(definition.image, options),
          resources: normalizeWorkspaceResources(definition.resources)
        }
      };
  }
}
function compileSchedule(definition, sandboxExports) {
  const schedule = definition.schedule;
  if (schedule === undefined)
    throw new Error("Task schedule is undefined");
  const sandbox = inspectSandboxDefinition(schedule.workspace.sandbox);
  if (sandbox === undefined) {
    throw new Error(`task ${JSON.stringify(definition.id)} schedule has an invalid Sandbox definition`);
  }
  const exported = sandboxExports.get(sandbox.id);
  if (exported === undefined) {
    throw new Error(`task ${JSON.stringify(definition.id)} schedule references unexported Sandbox ${JSON.stringify(sandbox.id)}`);
  }
  if (exported !== schedule.workspace.sandbox) {
    throw new Error(`task ${JSON.stringify(definition.id)} schedule references a different Sandbox object than the exported definition ${JSON.stringify(sandbox.id)}`);
  }
  return {
    cron: schedule.cron,
    timezone: schedule.timezone,
    workspace: {
      sandboxId: sandbox.id,
      secrets: schedule.workspace.secrets
    }
  };
}
function compileQueues(located, exports) {
  const queues = new Map;
  for (const item of exports) {
    if (isQueue(item.value)) {
      addQueue(queues, item.value, item.value);
    }
  }
  for (const { definition } of located) {
    if (definition.kind !== "task" && definition.kind !== "actor")
      continue;
    if (typeof definition.queue === "object") {
      addQueue(queues, definition.queue, definition.queue);
    } else if (definition.queue === undefined) {
      addQueue(queues, {
        name: `${definition.kind}/${definition.id}`
      }, definition);
    }
  }
  for (const { definition } of located) {
    if ((definition.kind === "task" || definition.kind === "actor") && typeof definition.queue === "string" && !queues.has(definition.queue)) {
      throw new Error(`${definition.kind} ${JSON.stringify(definition.id)} references undefined queue ${JSON.stringify(definition.queue)}`);
    }
  }
  if (queues.size > 1000)
    throw new Error("BuildPlan queues exceed 1000");
  return new Map([...queues].map(([name, entry]) => [name, entry.queue]));
}
function addQueue(queues, queue, owner) {
  validateQueueName(queue.name);
  const next = {
    name: queue.name,
    ...queue.concurrencyLimit === undefined || queue.concurrencyLimit === null ? {} : { concurrencyLimit: queue.concurrencyLimit }
  };
  const existing = queues.get(queue.name);
  if (existing !== undefined) {
    if (existing.owner === owner)
      return;
    throw new Error(`duplicate queue declaration ${JSON.stringify(queue.name)}`);
  }
  queues.set(queue.name, { owner, queue: next });
}
function normalizeRun(definition, kind, queues) {
  const queue = definition.queue === undefined ? `${kind}/${definition.id}` : typeof definition.queue === "string" ? definition.queue : definition.queue.name;
  if (!queues.has(queue)) {
    throw new Error(`${kind} ${JSON.stringify(definition.id)} queue is undefined`);
  }
  const maxDurationMs = definition.maxDuration === undefined ? 900000 : normalizeDuration(definition.maxDuration, `${kind} ${JSON.stringify(definition.id)} maxDuration`, 5000, 86400000);
  return {
    queue,
    maxDurationMs,
    retry: normalizeRetry(definition.retry),
    ...definition.ttl === undefined ? {} : {
      ttlMs: normalizeDuration(definition.ttl, `${kind} ${JSON.stringify(definition.id)} ttl`, 1, 31536000000)
    }
  };
}
function normalizeRetry(retry) {
  if (retry === undefined || retry.enabled === false) {
    return { enabled: false };
  }
  if (!Number.isInteger(retry.maxAttempts) || retry.maxAttempts < 1 || retry.maxAttempts > 10) {
    throw new Error("retry maxAttempts must be an integer in [1,10]");
  }
  const minMs = retry.backoff?.minDelay === undefined ? 1000 : normalizeDuration(retry.backoff.minDelay, "retry backoff minDelay", 1, 86400000);
  const maxMs = retry.backoff?.maxDelay === undefined ? 30000 : normalizeDuration(retry.backoff.maxDelay, "retry backoff maxDelay", 1, 86400000);
  const factor = retry.backoff?.factor ?? 2;
  const jitter = retry.backoff?.jitter ?? "full";
  if (minMs > maxMs) {
    throw new Error("retry backoff minDelay must not exceed maxDelay");
  }
  if (!Number.isSafeInteger(factor) || factor < 1 || factor > 100) {
    throw new Error("retry backoff factor must be an integer in [1,100]");
  }
  if (jitter !== "none" && jitter !== "full") {
    throw new Error("retry backoff jitter must be none or full");
  }
  return {
    enabled: true,
    maxAttempts: retry.maxAttempts,
    backoff: { minMs, maxMs, factor, jitter }
  };
}
function compileImageBuild(root, options) {
  const images = new Map;
  const visiting = new Set;
  const visit2 = (image) => {
    if (visiting.has(image.key)) {
      throw new Error(`image graph contains a cycle at ${JSON.stringify(image.key)}`);
    }
    const existing = images.get(image.key);
    if (existing !== undefined) {
      if (existing !== image) {
        throw new Error(`image key ${JSON.stringify(image.key)} is not unique`);
      }
      return;
    }
    visiting.add(image.key);
    images.set(image.key, image);
    for (const step of image.steps) {
      if (step.kind === "copy_from_image") {
        const source2 = inspectImage(step.source);
        if (source2 === undefined)
          throw new Error("invalid copyFrom image");
        visit2(source2);
      }
    }
    visiting.delete(image.key);
  };
  visit2(root);
  const specs = [...images.values()].sort((left, right) => compareUTF82(left.key, right.key)).map((image) => ({
    key: image.key,
    platform: {
      os: "linux",
      architecture: options.architecture
    },
    steps: image.steps.map((step) => compileImageStep(step, options))
  }));
  const stepCount = specs.reduce((total, image) => total + image.steps.length, 0);
  if (stepCount > 1e4)
    throw new Error("image build exceeds 10000 steps");
  return {
    formatVersion: 0,
    root: root.key,
    images: specs
  };
}
function compileImageStep(step, options) {
  switch (step.kind) {
    case "from":
      assertExactKeys(step, ["kind", "ref"], "image from step");
      return { from: { ref: step.ref } };
    case "run":
      assertExactKeys(step, ["argv", "kind"], "image run step");
      return {
        run: {
          argv: [...step.argv]
        }
      };
    case "copy_source_file":
      assertExactKeys(step, ["destination", "kind", "source"], "image source-file copy step");
      return {
        copySourceFile: {
          dst: step.destination,
          path: step.source.path
        }
      };
    case "copy_source_directory":
      assertExactKeys(step, ["destination", "kind", "source"], "image source-directory copy step");
      return {
        copySourceDir: {
          dst: step.destination,
          path: step.source.path
        }
      };
    case "copy_from_image": {
      assertExactKeys(step, ["destination", "kind", "source", "sourcePath"], "image cross-image copy step");
      const source2 = inspectImage(step.source);
      if (source2 === undefined)
        throw new Error("invalid copyFrom image");
      return {
        copyFromImage: {
          dst: step.destination,
          imageKey: source2.key,
          srcPath: step.sourcePath
        }
      };
    }
    case "workdir":
      assertExactKeys(step, ["kind", "path"], "image workdir step");
      return { workdir: { path: step.path } };
    case "env":
      assertExactKeys(step, ["key", "kind", "value"], "image env step");
      return { env: { key: step.key, value: step.value } };
    case "user":
      assertExactKeys(step, ["kind", "name"], "image user step");
      return { user: { name: step.name } };
  }
}
function assertExactKeys(value, expected, label) {
  const actual = Object.keys(value).sort(compareUTF82);
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} has unknown members`);
  }
}
function locatorEntry(item) {
  if (item.definition.kind === "sandbox") {
    throw new Error("Sandbox has no executable locator");
  }
  return {
    declaredId: item.definition.id,
    exportName: item.exportName,
    kind: item.definition.kind,
    modulePath: item.modulePath,
    slot: "handler"
  };
}
function programDeclaration(definition) {
  switch (definition.kind) {
    case "task":
      return {
        kind: "task",
        declaredId: definition.id,
        slots: definition.hasPayload ? ["handler", "payloadSchema"] : ["handler"]
      };
    case "actor":
      return {
        kind: "actor",
        declaredId: definition.id,
        slots: ["handler"]
      };
  }
}
function normalizeCpu(cpu) {
  if (!Number.isFinite(cpu) || cpu <= 0) {
    throw new Error("workspace cpu must be a finite positive number");
  }
  const text = cpu.toString();
  const match = /^(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$/i.exec(text);
  if (match === null)
    throw new Error("workspace cpu cannot be normalized");
  const integer = match[1];
  const fraction = match[2] ?? "";
  const exponent = Number(match[3] ?? "0");
  const significand = BigInt(`${integer}${fraction}`);
  const scale = exponent - fraction.length + 3;
  let milliCpu;
  if (scale >= 0) {
    milliCpu = significand * 10n ** BigInt(scale);
  } else {
    const divisor = 10n ** BigInt(-scale);
    if (significand % divisor !== 0n) {
      throw new Error("workspace cpu must resolve to whole milliCPU");
    }
    milliCpu = significand / divisor;
  }
  return safePositiveNumber(milliCpu, "workspace milliCPU");
}
function normalizeIecMiB(value, label) {
  const match = /^([1-9]\d*)(MiB|GiB)$/.exec(value);
  if (match === null) {
    throw new Error(`workspace ${label} must be a positive canonical integer suffixed by MiB or GiB`);
  }
  const result = BigInt(match[1]) * (match[2] === "GiB" ? 1024n : 1n);
  return safePositiveNumber(result, `workspace ${label} MiB`);
}
function normalizeDuration(value, label, minimumMs, maximumMs) {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value);
  if (match === null) {
    throw new Error(`${label} must match ^[1-9][0-9]*(ms|s|m|h|d)$`);
  }
  const multipliers = {
    ms: 1n,
    s: 1000n,
    m: 60000n,
    h: 3600000n,
    d: 86400000n
  };
  const milliseconds = BigInt(match[1]) * multipliers[match[2]];
  if (milliseconds < BigInt(minimumMs) || milliseconds > BigInt(maximumMs)) {
    throw new Error(`${label} must resolve to milliseconds in [${minimumMs},${maximumMs}]`);
  }
  return Number(milliseconds);
}
function safePositiveNumber(value, label) {
  if (value <= 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a positive safe integer`);
  }
  return Number(value);
}
function validateModulePath(path) {
  const suffixes = [
    ".cjs",
    ".cts",
    ".js",
    ".jsx",
    ".mjs",
    ".mts",
    ".ts",
    ".tsx"
  ];
  const components = path.split("/");
  if (path.length === 0 || !hasOnlyUnicodeScalarValues2(path) || path.startsWith("/") || path.includes("\\") || /[\p{Cc}]/u.test(path) || components.some((component) => component === "" || component === "." || component === "..") || components.includes("node_modules") || components[0] === "helmr" || path.endsWith(".d.ts") || path.endsWith(".d.mts") || path.endsWith(".d.cts") || !suffixes.some((suffix) => path.endsWith(suffix))) {
    throw new Error(`modulePath ${JSON.stringify(path)} is not an admitted first-party module path`);
  }
}
function validateExportName(name) {
  const length = new TextEncoder().encode(name).length;
  if (length < 1 || length > 256 || !hasOnlyUnicodeScalarValues2(name) || /[\p{Cc}]/u.test(name)) {
    throw new Error(`exportName ${JSON.stringify(name)} is invalid`);
  }
}
function compareLocatedDefinitions(left, right) {
  const order = {
    task: 0,
    actor: 1,
    sandbox: 2
  };
  return order[left.definition.kind] - order[right.definition.kind] || compareUTF82(left.definition.id, right.definition.id);
}
function compareLocatorOccurrence(left, right) {
  return compareUTF82(left.modulePath, right.modulePath) || compareUTF82(left.exportName, right.exportName);
}

// compiler/typescript/src/local-packages.ts
import { lstat as lstat2, readFile, readdir as readdir3, realpath as realpath3, stat as stat2 } from "node:fs/promises";
import { dirname as dirname2, relative as relative3, resolve as resolve4, sep as sep3 } from "node:path";

// node_modules/.bun/tinyglobby@0.2.17/node_modules/tinyglobby/dist/index.mjs
import { readdir as readdir2, readdirSync, realpath as realpath2, realpathSync, stat, statSync } from "fs";
import { isAbsolute, posix, resolve as resolve3 } from "path";
import { fileURLToPath } from "url";

// node_modules/.bun/fdir@6.5.0+5b2c60377f53a28e/node_modules/fdir/dist/index.mjs
import { createRequire } from "module";
import { basename, dirname, normalize, relative as relative2, resolve as resolve2, sep as sep2 } from "path";
import * as nativeFs from "fs";
var __require = /* @__PURE__ */ createRequire(import.meta.url);
function cleanPath(path) {
  let normalized = normalize(path);
  if (normalized.length > 1 && normalized[normalized.length - 1] === sep2)
    normalized = normalized.substring(0, normalized.length - 1);
  return normalized;
}
var SLASHES_REGEX = /[\\/]/g;
function convertSlashes(path, separator) {
  return path.replace(SLASHES_REGEX, separator);
}
var WINDOWS_ROOT_DIR_REGEX = /^[a-z]:[\\/]$/i;
function isRootDirectory(path) {
  return path === "/" || WINDOWS_ROOT_DIR_REGEX.test(path);
}
function normalizePath(path, options) {
  const { resolvePaths, normalizePath: normalizePath$1, pathSeparator } = options;
  const pathNeedsCleaning = process.platform === "win32" && path.includes("/") || path.startsWith(".");
  if (resolvePaths)
    path = resolve2(path);
  if (normalizePath$1 || pathNeedsCleaning)
    path = cleanPath(path);
  if (path === ".")
    return "";
  const needsSeperator = path[path.length - 1] !== pathSeparator;
  return convertSlashes(needsSeperator ? path + pathSeparator : path, pathSeparator);
}
function joinPathWithBasePath(filename, directoryPath) {
  return directoryPath + filename;
}
function joinPathWithRelativePath(root, options) {
  return function(filename, directoryPath) {
    const sameRoot = directoryPath.startsWith(root);
    if (sameRoot)
      return directoryPath.slice(root.length) + filename;
    else
      return convertSlashes(relative2(root, directoryPath), options.pathSeparator) + options.pathSeparator + filename;
  };
}
function joinPath(filename) {
  return filename;
}
function joinDirectoryPath(filename, directoryPath, separator) {
  return directoryPath + filename + separator;
}
function build$7(root, options) {
  const { relativePaths, includeBasePath } = options;
  return relativePaths && root ? joinPathWithRelativePath(root, options) : includeBasePath ? joinPathWithBasePath : joinPath;
}
function pushDirectoryWithRelativePath(root) {
  return function(directoryPath, paths) {
    paths.push(directoryPath.substring(root.length) || ".");
  };
}
function pushDirectoryFilterWithRelativePath(root) {
  return function(directoryPath, paths, filters) {
    const relativePath = directoryPath.substring(root.length) || ".";
    if (filters.every((filter) => filter(relativePath, true)))
      paths.push(relativePath);
  };
}
var pushDirectory = (directoryPath, paths) => {
  paths.push(directoryPath || ".");
};
var pushDirectoryFilter = (directoryPath, paths, filters) => {
  const path = directoryPath || ".";
  if (filters.every((filter) => filter(path, true)))
    paths.push(path);
};
var empty$2 = () => {};
function build$6(root, options) {
  const { includeDirs, filters, relativePaths } = options;
  if (!includeDirs)
    return empty$2;
  if (relativePaths)
    return filters && filters.length ? pushDirectoryFilterWithRelativePath(root) : pushDirectoryWithRelativePath(root);
  return filters && filters.length ? pushDirectoryFilter : pushDirectory;
}
var pushFileFilterAndCount = (filename, _paths, counts, filters) => {
  if (filters.every((filter) => filter(filename, false)))
    counts.files++;
};
var pushFileFilter = (filename, paths, _counts, filters) => {
  if (filters.every((filter) => filter(filename, false)))
    paths.push(filename);
};
var pushFileCount = (_filename, _paths, counts, _filters) => {
  counts.files++;
};
var pushFile = (filename, paths) => {
  paths.push(filename);
};
var empty$1 = () => {};
function build$5(options) {
  const { excludeFiles, filters, onlyCounts } = options;
  if (excludeFiles)
    return empty$1;
  if (filters && filters.length)
    return onlyCounts ? pushFileFilterAndCount : pushFileFilter;
  else if (onlyCounts)
    return pushFileCount;
  else
    return pushFile;
}
var getArray = (paths) => {
  return paths;
};
var getArrayGroup = () => {
  return [""].slice(0, 0);
};
function build$4(options) {
  return options.group ? getArrayGroup : getArray;
}
var groupFiles = (groups, directory, files) => {
  groups.push({
    directory,
    files,
    dir: directory
  });
};
var empty = () => {};
function build$3(options) {
  return options.group ? groupFiles : empty;
}
var resolveSymlinksAsync = function(path, state, callback$1) {
  const { queue, fs, options: { suppressErrors } } = state;
  queue.enqueue();
  fs.realpath(path, (error, resolvedPath) => {
    if (error)
      return queue.dequeue(suppressErrors ? null : error, state);
    fs.stat(resolvedPath, (error$1, stat) => {
      if (error$1)
        return queue.dequeue(suppressErrors ? null : error$1, state);
      if (stat.isDirectory() && isRecursive(path, resolvedPath, state))
        return queue.dequeue(null, state);
      callback$1(stat, resolvedPath);
      queue.dequeue(null, state);
    });
  });
};
var resolveSymlinks = function(path, state, callback$1) {
  const { queue, fs, options: { suppressErrors } } = state;
  queue.enqueue();
  try {
    const resolvedPath = fs.realpathSync(path);
    const stat = fs.statSync(resolvedPath);
    if (stat.isDirectory() && isRecursive(path, resolvedPath, state))
      return;
    callback$1(stat, resolvedPath);
  } catch (e) {
    if (!suppressErrors)
      throw e;
  }
};
function build$2(options, isSynchronous) {
  if (!options.resolveSymlinks || options.excludeSymlinks)
    return null;
  return isSynchronous ? resolveSymlinks : resolveSymlinksAsync;
}
function isRecursive(path, resolved, state) {
  if (state.options.useRealPaths)
    return isRecursiveUsingRealPaths(resolved, state);
  let parent = dirname(path);
  let depth = 1;
  while (parent !== state.root && depth < 2) {
    const resolvedPath = state.symlinks.get(parent);
    const isSameRoot = !!resolvedPath && (resolvedPath === resolved || resolvedPath.startsWith(resolved) || resolved.startsWith(resolvedPath));
    if (isSameRoot)
      depth++;
    else
      parent = dirname(parent);
  }
  state.symlinks.set(path, resolved);
  return depth > 1;
}
function isRecursiveUsingRealPaths(resolved, state) {
  return state.visited.includes(resolved + state.options.pathSeparator);
}
var onlyCountsSync = (state) => {
  return state.counts;
};
var groupsSync = (state) => {
  return state.groups;
};
var defaultSync = (state) => {
  return state.paths;
};
var limitFilesSync = (state) => {
  return state.paths.slice(0, state.options.maxFiles);
};
var onlyCountsAsync = (state, error, callback$1) => {
  report(error, callback$1, state.counts, state.options.suppressErrors);
  return null;
};
var defaultAsync = (state, error, callback$1) => {
  report(error, callback$1, state.paths, state.options.suppressErrors);
  return null;
};
var limitFilesAsync = (state, error, callback$1) => {
  report(error, callback$1, state.paths.slice(0, state.options.maxFiles), state.options.suppressErrors);
  return null;
};
var groupsAsync = (state, error, callback$1) => {
  report(error, callback$1, state.groups, state.options.suppressErrors);
  return null;
};
function report(error, callback$1, output, suppressErrors) {
  if (error && !suppressErrors)
    callback$1(error, output);
  else
    callback$1(null, output);
}
function build$1(options, isSynchronous) {
  const { onlyCounts, group, maxFiles } = options;
  if (onlyCounts)
    return isSynchronous ? onlyCountsSync : onlyCountsAsync;
  else if (group)
    return isSynchronous ? groupsSync : groupsAsync;
  else if (maxFiles)
    return isSynchronous ? limitFilesSync : limitFilesAsync;
  else
    return isSynchronous ? defaultSync : defaultAsync;
}
var readdirOpts = { withFileTypes: true };
var walkAsync = (state, crawlPath, directoryPath, currentDepth, callback$1) => {
  state.queue.enqueue();
  if (currentDepth < 0)
    return state.queue.dequeue(null, state);
  const { fs } = state;
  state.visited.push(crawlPath);
  state.counts.directories++;
  fs.readdir(crawlPath || ".", readdirOpts, (error, entries = []) => {
    callback$1(entries, directoryPath, currentDepth);
    state.queue.dequeue(state.options.suppressErrors ? null : error, state);
  });
};
var walkSync = (state, crawlPath, directoryPath, currentDepth, callback$1) => {
  const { fs } = state;
  if (currentDepth < 0)
    return;
  state.visited.push(crawlPath);
  state.counts.directories++;
  let entries = [];
  try {
    entries = fs.readdirSync(crawlPath || ".", readdirOpts);
  } catch (e) {
    if (!state.options.suppressErrors)
      throw e;
  }
  callback$1(entries, directoryPath, currentDepth);
};
function build(isSynchronous) {
  return isSynchronous ? walkSync : walkAsync;
}
var Queue = class {
  count = 0;
  constructor(onQueueEmpty) {
    this.onQueueEmpty = onQueueEmpty;
  }
  enqueue() {
    this.count++;
    return this.count;
  }
  dequeue(error, output) {
    if (this.onQueueEmpty && (--this.count <= 0 || error)) {
      this.onQueueEmpty(error, output);
      if (error) {
        output.controller.abort();
        this.onQueueEmpty = undefined;
      }
    }
  }
};
var Counter = class {
  _files = 0;
  _directories = 0;
  set files(num) {
    this._files = num;
  }
  get files() {
    return this._files;
  }
  set directories(num) {
    this._directories = num;
  }
  get directories() {
    return this._directories;
  }
  get dirs() {
    return this._directories;
  }
};
var Aborter = class {
  aborted = false;
  abort() {
    this.aborted = true;
  }
};
var Walker = class {
  root;
  isSynchronous;
  state;
  joinPath;
  pushDirectory;
  pushFile;
  getArray;
  groupFiles;
  resolveSymlink;
  walkDirectory;
  callbackInvoker;
  constructor(root, options, callback$1) {
    this.isSynchronous = !callback$1;
    this.callbackInvoker = build$1(options, this.isSynchronous);
    this.root = normalizePath(root, options);
    this.state = {
      root: isRootDirectory(this.root) ? this.root : this.root.slice(0, -1),
      paths: [""].slice(0, 0),
      groups: [],
      counts: new Counter,
      options,
      queue: new Queue((error, state) => this.callbackInvoker(state, error, callback$1)),
      symlinks: /* @__PURE__ */ new Map,
      visited: [""].slice(0, 0),
      controller: new Aborter,
      fs: options.fs || nativeFs
    };
    this.joinPath = build$7(this.root, options);
    this.pushDirectory = build$6(this.root, options);
    this.pushFile = build$5(options);
    this.getArray = build$4(options);
    this.groupFiles = build$3(options);
    this.resolveSymlink = build$2(options, this.isSynchronous);
    this.walkDirectory = build(this.isSynchronous);
  }
  start() {
    this.pushDirectory(this.root, this.state.paths, this.state.options.filters);
    this.walkDirectory(this.state, this.root, this.root, this.state.options.maxDepth, this.walk);
    return this.isSynchronous ? this.callbackInvoker(this.state, null) : null;
  }
  walk = (entries, directoryPath, depth) => {
    const { paths, options: { filters, resolveSymlinks: resolveSymlinks$1, excludeSymlinks, exclude, maxFiles, signal, useRealPaths, pathSeparator }, controller } = this.state;
    if (controller.aborted || signal && signal.aborted || maxFiles && paths.length > maxFiles)
      return;
    const files = this.getArray(this.state.paths);
    for (let i = 0;i < entries.length; ++i) {
      const entry = entries[i];
      if (entry.isFile() || entry.isSymbolicLink() && !resolveSymlinks$1 && !excludeSymlinks) {
        const filename = this.joinPath(entry.name, directoryPath);
        this.pushFile(filename, files, this.state.counts, filters);
      } else if (entry.isDirectory()) {
        let path = joinDirectoryPath(entry.name, directoryPath, this.state.options.pathSeparator);
        if (exclude && exclude(entry.name, path))
          continue;
        this.pushDirectory(path, paths, filters);
        this.walkDirectory(this.state, path, path, depth - 1, this.walk);
      } else if (this.resolveSymlink && entry.isSymbolicLink()) {
        let path = joinPathWithBasePath(entry.name, directoryPath);
        this.resolveSymlink(path, this.state, (stat, resolvedPath) => {
          if (stat.isDirectory()) {
            resolvedPath = normalizePath(resolvedPath, this.state.options);
            if (exclude && exclude(entry.name, useRealPaths ? resolvedPath : path + pathSeparator))
              return;
            this.walkDirectory(this.state, resolvedPath, useRealPaths ? resolvedPath : path + pathSeparator, depth - 1, this.walk);
          } else {
            resolvedPath = useRealPaths ? resolvedPath : path;
            const filename = basename(resolvedPath);
            const directoryPath$1 = normalizePath(dirname(resolvedPath), this.state.options);
            resolvedPath = this.joinPath(filename, directoryPath$1);
            this.pushFile(resolvedPath, files, this.state.counts, filters);
          }
        });
      }
    }
    this.groupFiles(this.state.groups, directoryPath, files);
  };
};
function promise(root, options) {
  return new Promise((resolve$1, reject) => {
    callback(root, options, (err, output) => {
      if (err)
        return reject(err);
      resolve$1(output);
    });
  });
}
function callback(root, options, callback$1) {
  let walker = new Walker(root, options, callback$1);
  walker.start();
}
function sync(root, options) {
  const walker = new Walker(root, options);
  return walker.start();
}
var APIBuilder = class {
  constructor(root, options) {
    this.root = root;
    this.options = options;
  }
  withPromise() {
    return promise(this.root, this.options);
  }
  withCallback(cb) {
    callback(this.root, this.options, cb);
  }
  sync() {
    return sync(this.root, this.options);
  }
};
var pm = null;
try {
  __require.resolve("picomatch");
  pm = __require("picomatch");
} catch {}
var Builder = class {
  globCache = {};
  options = {
    maxDepth: Infinity,
    suppressErrors: true,
    pathSeparator: sep2,
    filters: []
  };
  globFunction;
  constructor(options) {
    this.options = {
      ...this.options,
      ...options
    };
    this.globFunction = this.options.globFunction;
  }
  group() {
    this.options.group = true;
    return this;
  }
  withPathSeparator(separator) {
    this.options.pathSeparator = separator;
    return this;
  }
  withBasePath() {
    this.options.includeBasePath = true;
    return this;
  }
  withRelativePaths() {
    this.options.relativePaths = true;
    return this;
  }
  withDirs() {
    this.options.includeDirs = true;
    return this;
  }
  withMaxDepth(depth) {
    this.options.maxDepth = depth;
    return this;
  }
  withMaxFiles(limit) {
    this.options.maxFiles = limit;
    return this;
  }
  withFullPaths() {
    this.options.resolvePaths = true;
    this.options.includeBasePath = true;
    return this;
  }
  withErrors() {
    this.options.suppressErrors = false;
    return this;
  }
  withSymlinks({ resolvePaths = true } = {}) {
    this.options.resolveSymlinks = true;
    this.options.useRealPaths = resolvePaths;
    return this.withFullPaths();
  }
  withAbortSignal(signal) {
    this.options.signal = signal;
    return this;
  }
  normalize() {
    this.options.normalizePath = true;
    return this;
  }
  filter(predicate) {
    this.options.filters.push(predicate);
    return this;
  }
  onlyDirs() {
    this.options.excludeFiles = true;
    this.options.includeDirs = true;
    return this;
  }
  exclude(predicate) {
    this.options.exclude = predicate;
    return this;
  }
  onlyCounts() {
    this.options.onlyCounts = true;
    return this;
  }
  crawl(root) {
    return new APIBuilder(root || ".", this.options);
  }
  withGlobFunction(fn) {
    this.globFunction = fn;
    return this;
  }
  crawlWithOptions(root, options) {
    this.options = {
      ...this.options,
      ...options
    };
    return new APIBuilder(root || ".", this.options);
  }
  glob(...patterns) {
    if (this.globFunction)
      return this.globWithOptions(patterns);
    return this.globWithOptions(patterns, ...[{ dot: true }]);
  }
  globWithOptions(patterns, ...options) {
    const globFn = this.globFunction || pm;
    if (!globFn)
      throw new Error("Please specify a glob function to use glob matching.");
    var isMatch = this.globCache[patterns.join("\x00")];
    if (!isMatch) {
      isMatch = globFn(patterns, ...options);
      this.globCache[patterns.join("\x00")] = isMatch;
    }
    this.options.filters.push((path) => isMatch(path));
    return this;
  }
};

// node_modules/.bun/tinyglobby@0.2.17/node_modules/tinyglobby/dist/index.mjs
var import_picomatch = __toESM(require_picomatch2(), 1);
var isReadonlyArray = Array.isArray;
var BACKSLASHES = /\\/g;
var DRIVE_RELATIVE_PATH = /^[A-Za-z]:$/;
var isWin = process.platform === "win32";
var ONLY_PARENT_DIRECTORIES = /^(\/?\.\.)+$/;
function getPartialMatcher(patterns, options = {}) {
  const patternsCount = patterns.length;
  const patternsParts = Array(patternsCount);
  const matchers = Array(patternsCount);
  let i, j;
  for (i = 0;i < patternsCount; i++) {
    const parts = splitPattern(patterns[i]);
    patternsParts[i] = parts;
    const partsCount = parts.length;
    const partMatchers = Array(partsCount);
    for (j = 0;j < partsCount; j++)
      partMatchers[j] = import_picomatch.default(parts[j], options);
    matchers[i] = partMatchers;
  }
  return (input) => {
    const inputParts = input.split("/");
    if (inputParts[0] === ".." && ONLY_PARENT_DIRECTORIES.test(input))
      return true;
    for (i = 0;i < patternsCount; i++) {
      const patternParts = patternsParts[i];
      const matcher = matchers[i];
      const inputPatternCount = inputParts.length;
      const minParts = Math.min(inputPatternCount, patternParts.length);
      j = 0;
      while (j < minParts) {
        const part = patternParts[j];
        if (part.includes("/"))
          return true;
        if (!matcher[j](inputParts[j]))
          break;
        if (!options.noglobstar && part === "**")
          return true;
        j++;
      }
      if (j === inputPatternCount)
        return true;
    }
    return false;
  };
}
var WIN32_ROOT_DIR = /^[A-Z]:\/$/i;
var isRoot = isWin ? (p) => WIN32_ROOT_DIR.test(p) : (p) => p === "/";
function buildFormat(cwd, root, absolute) {
  if (cwd === root || root.startsWith(`${cwd}/`)) {
    if (absolute) {
      const start = cwd.length + +!isRoot(cwd);
      return (p, isDir) => p.slice(start, isDir ? -1 : undefined) || ".";
    }
    const prefix = root.slice(cwd.length + 1);
    if (prefix)
      return (p, isDir) => {
        if (p === ".")
          return prefix;
        const result = `${prefix}/${p}`;
        return isDir ? result.slice(0, -1) : result;
      };
    return (p, isDir) => isDir && p !== "." ? p.slice(0, -1) : p;
  }
  if (absolute)
    return (p) => posix.relative(cwd, p) || ".";
  return (p) => posix.relative(cwd, `${root}/${p}`) || ".";
}
function buildRelative(cwd, root) {
  if (root.startsWith(`${cwd}/`)) {
    const prefix = root.slice(cwd.length + 1);
    return (p) => `${prefix}/${p}`;
  }
  return (p) => {
    const result = posix.relative(cwd, `${root}/${p}`);
    return p[p.length - 1] === "/" && result !== "" ? `${result}/` : result || ".";
  };
}
function ensureNonDriveRelativePath(path) {
  return path.replace(DRIVE_RELATIVE_PATH, (match) => `${match}/`);
}
var splitPatternOptions = { parts: true };
function splitPattern(path) {
  var _result$parts;
  const result = import_picomatch.default.scan(path, splitPatternOptions);
  return ((_result$parts = result.parts) === null || _result$parts === undefined ? undefined : _result$parts.length) ? result.parts : [path];
}
var POSIX_UNESCAPED_GLOB_SYMBOLS = /(?<!\\)([()[\]{}*?|]|^!|[!+@](?=\()|\\(?![()[\]{}!*+?@|]))/g;
var WIN32_UNESCAPED_GLOB_SYMBOLS = /(?<!\\)([()[\]{}]|^!|[!+@](?=\())/g;
var escapePosixPath = (path) => path.replace(POSIX_UNESCAPED_GLOB_SYMBOLS, "\\$&");
var escapeWin32Path = (path) => path.replace(WIN32_UNESCAPED_GLOB_SYMBOLS, "\\$&");
var escapePath = isWin ? escapeWin32Path : escapePosixPath;
function isDynamicPattern(pattern, options) {
  if ((options === null || options === undefined ? undefined : options.caseSensitiveMatch) === false)
    return true;
  const scan = import_picomatch.default.scan(pattern);
  return scan.isGlob || scan.negated;
}
function log(...tasks) {
  console.log(`[tinyglobby ${(/* @__PURE__ */ new Date()).toLocaleTimeString("es")}]`, ...tasks);
}
function ensureStringArray(value) {
  return typeof value === "string" ? [value] : value !== null && value !== undefined ? value : [];
}
var PARENT_DIRECTORY = /^(\/?\.\.)+/;
var ESCAPING_BACKSLASHES = /\\(?=[()[\]{}!*+?@|])/g;
function normalizePattern(pattern, opts, props, isIgnore) {
  var _PARENT_DIRECTORY$exe;
  const cwd = opts.cwd;
  let result = pattern;
  if (pattern[pattern.length - 1] === "/")
    result = pattern.slice(0, -1);
  if (result[result.length - 1] !== "*" && opts.expandDirectories)
    result += "/**";
  const escapedCwd = escapePath(cwd);
  result = isAbsolute(result.replace(ESCAPING_BACKSLASHES, "")) ? posix.relative(escapedCwd, result) : posix.normalize(result);
  const parentDir = (_PARENT_DIRECTORY$exe = PARENT_DIRECTORY.exec(result)) === null || _PARENT_DIRECTORY$exe === undefined ? undefined : _PARENT_DIRECTORY$exe[0];
  const parts = splitPattern(result);
  if (parentDir) {
    const n = (parentDir.length + 1) / 3;
    let i = 0;
    const cwdParts = escapedCwd.split("/");
    while (i < n && parts[i + n] === cwdParts[cwdParts.length + i - n]) {
      result = result.slice(0, (n - i - 1) * 3) + result.slice((n - i) * 3 + parts[i + n].length + 1) || ".";
      i++;
    }
    const potentialRoot = posix.join(cwd, parentDir.slice(i * 3));
    if (potentialRoot[0] !== "." && props.root.length > potentialRoot.length) {
      props.root = ensureNonDriveRelativePath(potentialRoot);
      props.depthOffset = -n + i;
    }
  }
  if (!isIgnore && props.depthOffset >= 0) {
    var _props$commonPath;
    (_props$commonPath = props.commonPath) !== null && _props$commonPath !== undefined || (props.commonPath = parts);
    const newCommonPath = [];
    const length = Math.min(props.commonPath.length, parts.length);
    for (let i = 0;i < length; i++) {
      const part = parts[i];
      if (part === "**" && !parts[i + 1]) {
        newCommonPath.pop();
        break;
      }
      if (i === parts.length - 1 || part !== props.commonPath[i] || isDynamicPattern(part))
        break;
      newCommonPath.push(part);
    }
    props.depthOffset = newCommonPath.length;
    props.commonPath = newCommonPath;
    props.root = ensureNonDriveRelativePath(newCommonPath.length > 0 ? posix.join(cwd, ...newCommonPath) : cwd);
  }
  return result;
}
function processPatterns(options, patterns, props) {
  const matchPatterns = [];
  const ignorePatterns = [];
  for (const pattern of options.ignore) {
    if (!pattern)
      continue;
    if (pattern[0] !== "!" || pattern[1] === "(")
      ignorePatterns.push(normalizePattern(pattern, options, props, true));
  }
  for (const pattern of patterns) {
    if (!pattern)
      continue;
    if (pattern[0] !== "!" || pattern[1] === "(")
      matchPatterns.push(normalizePattern(pattern, options, props, false));
    else if (pattern[1] !== "!" || pattern[2] === "(")
      ignorePatterns.push(normalizePattern(pattern.slice(1), options, props, true));
  }
  return {
    match: matchPatterns,
    ignore: ignorePatterns
  };
}
function buildCrawler(options, patterns) {
  const cwd = options.cwd;
  const props = {
    root: cwd,
    depthOffset: 0
  };
  const processed = processPatterns(options, patterns, props);
  if (options.debug)
    log("internal processing patterns:", processed);
  const { absolute, caseSensitiveMatch, debug, dot, followSymbolicLinks, onlyDirectories } = options;
  const root = props.root.replace(BACKSLASHES, "");
  const matchOptions = {
    dot,
    nobrace: options.braceExpansion === false,
    nocase: !caseSensitiveMatch,
    noextglob: options.extglob === false,
    noglobstar: options.globstar === false,
    posix: true
  };
  const matcher = import_picomatch.default(processed.match, matchOptions);
  const ignore = import_picomatch.default(processed.ignore, matchOptions);
  const partialMatcher = getPartialMatcher(processed.match, matchOptions);
  const format2 = buildFormat(cwd, root, absolute);
  const excludeFormatter = absolute ? format2 : buildFormat(cwd, root, true);
  const excludePredicate = (_, p) => {
    const relativePath = excludeFormatter(p, true);
    return relativePath !== "." && !partialMatcher(relativePath) || ignore(relativePath);
  };
  let maxDepth;
  if (options.deep !== undefined)
    maxDepth = Math.round(options.deep - props.depthOffset);
  const crawler = new Builder({
    filters: [debug ? (p, isDirectory) => {
      const path = format2(p, isDirectory);
      const matches = matcher(path) && !ignore(path);
      if (matches)
        log(`matched ${path}`);
      return matches;
    } : (p, isDirectory) => {
      const path = format2(p, isDirectory);
      return matcher(path) && !ignore(path);
    }],
    exclude: debug ? (_, p) => {
      const skipped = excludePredicate(_, p);
      log(`${skipped ? "skipped" : "crawling"} ${p}`);
      return skipped;
    } : excludePredicate,
    fs: options.fs,
    pathSeparator: "/",
    relativePaths: !absolute,
    resolvePaths: absolute,
    includeBasePath: absolute,
    resolveSymlinks: followSymbolicLinks,
    excludeSymlinks: !followSymbolicLinks,
    excludeFiles: onlyDirectories,
    includeDirs: onlyDirectories || !options.onlyFiles,
    maxDepth,
    signal: options.signal
  }).crawl(root);
  if (options.debug)
    log("internal properties:", {
      ...props,
      root
    });
  return [crawler, cwd !== root && !absolute && buildRelative(cwd, root)];
}
function formatPaths(paths, mapper) {
  if (mapper)
    for (let i = paths.length - 1;i >= 0; i--)
      paths[i] = mapper(paths[i]);
  return paths;
}
var defaultOptions = {
  caseSensitiveMatch: true,
  debug: !!process.env.TINYGLOBBY_DEBUG,
  expandDirectories: true,
  followSymbolicLinks: true,
  onlyFiles: true
};
function getOptions(options) {
  const opts = Object.assign({}, options);
  for (const key in defaultOptions)
    if (opts[key] === undefined)
      Object.assign(opts, { [key]: defaultOptions[key] });
  opts.cwd = (opts.cwd instanceof URL ? fileURLToPath(opts.cwd) : resolve3(opts.cwd || process.cwd())).replace(BACKSLASHES, "/");
  opts.ignore = ensureStringArray(opts.ignore);
  opts.fs && (opts.fs = {
    readdir: opts.fs.readdir || readdir2,
    readdirSync: opts.fs.readdirSync || readdirSync,
    realpath: opts.fs.realpath || realpath2,
    realpathSync: opts.fs.realpathSync || realpathSync,
    stat: opts.fs.stat || stat,
    statSync: opts.fs.statSync || statSync
  });
  if (opts.debug)
    log("globbing with options:", opts);
  return opts;
}
function getCrawler(globInput, inputOptions = {}) {
  var _ref;
  if (globInput && (inputOptions === null || inputOptions === undefined ? undefined : inputOptions.patterns))
    throw new Error("Cannot pass patterns as both an argument and an option");
  const isModern = isReadonlyArray(globInput) || typeof globInput === "string";
  const patterns = ensureStringArray((_ref = isModern ? globInput : globInput.patterns) !== null && _ref !== undefined ? _ref : "**/*");
  const options = getOptions(isModern ? inputOptions : globInput);
  return patterns.length > 0 ? buildCrawler(options, patterns) : [];
}
async function glob(globInput, options) {
  const [crawler, relative3] = getCrawler(globInput, options);
  return crawler ? formatPaths(await crawler.withPromise(), relative3) : [];
}

// compiler/typescript/src/local-packages.ts
async function deriveLocalPackages(root) {
  const canonicalRoot = await realpath3(root);
  const sourceRoots = new Set([""]);
  const rootManifest = await packageManifest(canonicalRoot);
  const patterns = workspacePatterns(rootManifest);
  if (patterns.length !== 0) {
    const manifests = await glob(patterns.map((pattern) => `${stripTrailingSlash(pattern)}/package.json`), {
      absolute: false,
      cwd: canonicalRoot,
      followSymbolicLinks: false,
      ignore: ["**/node_modules/**", "helmr/**"],
      onlyFiles: true
    });
    for (const manifest of manifests) {
      sourceRoots.add(projectPath2(canonicalRoot, dirname2(resolve4(canonicalRoot, manifest))));
    }
  }
  const pending = [...sourceRoots];
  const inspected = new Set;
  while (pending.length !== 0) {
    const sourceRoot = pending.shift();
    if (inspected.has(sourceRoot))
      continue;
    inspected.add(sourceRoot);
    const manifest = await packageManifest(resolve4(canonicalRoot, sourceRoot));
    const targets = localDependencyTargets(manifest).map((dependency) => ({
      label: dependency,
      path: resolve4(canonicalRoot, sourceRoot, dependency)
    }));
    targets.push(...await linkedLocalPackageTargets(canonicalRoot, sourceRoot));
    for (const targetInput of targets) {
      const target = await realpath3(targetInput.path);
      if (!(await stat2(target)).isDirectory()) {
        throw new Error(`local package target ${JSON.stringify(targetInput.label)} is not a directory`);
      }
      const path = projectPath2(canonicalRoot, target);
      if (!inside2(path) || hasNodeModules(path) || path.startsWith("helmr/")) {
        throw new Error(`local package target ${JSON.stringify(targetInput.label)} escapes project source`);
      }
      if (!sourceRoots.has(path)) {
        sourceRoots.add(path);
        pending.push(path);
      }
    }
  }
  const byName = new Map;
  for (const sourceRoot of [...sourceRoots].sort(compareUTF82)) {
    const manifest = await packageManifest(resolve4(canonicalRoot, sourceRoot));
    const name = manifest["name"];
    if (sourceRoot === "" && typeof name !== "string")
      continue;
    if (typeof name !== "string" || name === "") {
      throw new Error(`local package ${JSON.stringify(sourceRoot)} has no name`);
    }
    const previous = byName.get(name);
    if (previous !== undefined && previous !== sourceRoot) {
      throw new Error(`local package name ${JSON.stringify(name)} is ambiguous`);
    }
    byName.set(name, sourceRoot);
  }
  const packages = new Map;
  for (const [name, sourceRoot] of byName) {
    for (const importerRoot of sourceRoots) {
      let directory = resolve4(canonicalRoot, importerRoot);
      for (;; ) {
        const installedRoot = resolve4(directory, "node_modules", name);
        try {
          const installedManifest = await packageManifest(installedRoot);
          if (installedManifest["name"] !== name) {
            throw new Error(`installed local package ${JSON.stringify(installedRoot)} has the wrong name`);
          }
          const installed = projectPath2(canonicalRoot, installedRoot);
          if (!inside2(installed) || !hasNodeModules(installed)) {
            throw new Error(`installed local package ${JSON.stringify(name)} escapes project`);
          }
          packages.set(installed, { installedRoot: installed, name, sourceRoot });
        } catch (error) {
          if (error.code !== "ENOENT")
            throw error;
        }
        if (directory === canonicalRoot)
          break;
        const parent = dirname2(directory);
        if (!inside2(relative3(canonicalRoot, parent)))
          break;
        directory = parent;
      }
    }
  }
  return [...packages.values()].sort((left, right) => compareUTF82(left.installedRoot, right.installedRoot));
}
async function linkedLocalPackageTargets(root, importerRoot) {
  const modules = resolve4(root, importerRoot, "node_modules");
  let entries;
  try {
    entries = await readdir3(modules, { withFileTypes: true });
  } catch (error) {
    if (error.code === "ENOENT")
      return [];
    throw error;
  }
  const candidates = [];
  for (const entry of entries.sort((left, right) => compareUTF82(left.name, right.name))) {
    const path = resolve4(modules, entry.name);
    if (entry.name.startsWith("@") && entry.isDirectory()) {
      const scoped = await readdir3(path, { withFileTypes: true });
      for (const child of scoped.sort((left, right) => compareUTF82(left.name, right.name))) {
        const childPath = resolve4(path, child.name);
        if ((await lstat2(childPath)).isSymbolicLink()) {
          await addLinkedLocalPackageTarget(candidates, root, `${entry.name}/${child.name}`, childPath);
        }
      }
      continue;
    }
    if ((await lstat2(path)).isSymbolicLink()) {
      await addLinkedLocalPackageTarget(candidates, root, entry.name, path);
    }
  }
  return candidates;
}
async function addLinkedLocalPackageTarget(candidates, root, label, path) {
  const target = projectPath2(root, await realpath3(path));
  if (inside2(target) && !hasNodeModules(target) && !target.startsWith("helmr/")) {
    candidates.push({ label, path });
  }
}
function workspacePatterns(manifest) {
  const workspaces2 = manifest["workspaces"];
  if (workspaces2 === undefined)
    return [];
  if (Array.isArray(workspaces2)) {
    return stringArray(workspaces2, "package.json workspaces");
  }
  if (typeof workspaces2 === "object" && workspaces2 !== null) {
    return stringArray(workspaces2["packages"], "package.json workspaces.packages");
  }
  throw new Error("package.json workspaces must be an array or object");
}
function stringArray(value, name) {
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string" || item === "")) {
    throw new Error(`${name} must be an array of non-empty strings`);
  }
  return [...new Set(value)].sort(compareUTF82);
}
function localDependencyTargets(manifest) {
  const targets = new Set;
  for (const field of [
    "dependencies",
    "devDependencies",
    "optionalDependencies",
    "peerDependencies"
  ]) {
    const dependencies = manifest[field];
    if (typeof dependencies !== "object" || dependencies === null || Array.isArray(dependencies)) {
      continue;
    }
    for (const value of Object.values(dependencies)) {
      if (typeof value === "string" && (value.startsWith("file:") || value.startsWith("link:"))) {
        targets.add(value.slice(value.indexOf(":") + 1));
      }
    }
  }
  return [...targets].sort(compareUTF82);
}
async function packageManifest(root) {
  const value = JSON.parse(await readFile(resolve4(root, "package.json"), "utf8"));
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`package manifest at ${JSON.stringify(root)} is not an object`);
  }
  return value;
}
function stripTrailingSlash(value) {
  return value.replace(/\/+$/, "");
}
function projectPath2(root, value) {
  return relative3(root, value).split(sep3).join("/");
}
function hasNodeModules(path) {
  return path.split("/").includes("node_modules");
}
function inside2(path) {
  return path === "" || path !== ".." && !path.startsWith("../") && !path.startsWith("/");
}

// compiler/typescript/src/bundle.ts
var COMPILER_API_VERSION = "helmr.compiler.v0";
var ESBUILD_VERSION = "0.28.1";
var RUNTIME_PROGRAM_ROOT = "/opt/helmr/program";
if (esbuildVersion !== ESBUILD_VERSION) {
  throw new Error(`esbuild version ${JSON.stringify(esbuildVersion)} does not match ${ESBUILD_VERSION}`);
}
var declarationExtensions = [
  ".cjs",
  ".cts",
  ".js",
  ".jsx",
  ".mjs",
  ".mts",
  ".ts",
  ".tsx"
];
function compilerContract() {
  return {
    apiVersion: COMPILER_API_VERSION,
    esbuildVersion: ESBUILD_VERSION,
    optionsContractDigest: compilerOptionsContractDigest(),
    output: {
      aggregate: "analysis-only",
      finalModules: "independent",
      sharedChunks: false,
      sourceMaps: "external"
    },
    source: {
      declarationExtensions,
      packageDependencies: "external",
      semantics: "pinned-esbuild",
      workspaceDependencies: "bundled"
    }
  };
}
async function compileProgram(options) {
  const root = await realpath4(options.root);
  const outputRoot = resolve5(options.outputRoot);
  const canonicalLocalPackages = await deriveLocalPackages(root);
  const modules = await discoverModules(root, options.config);
  if (modules.length === 0) {
    throw new Error("configured dirs contain no declaration source modules");
  }
  const analysisRoot = resolve5(outputRoot, "analysis");
  await mkdir(analysisRoot, { recursive: false });
  try {
    const aggregatePath = resolve5(analysisRoot, "aggregate.mjs");
    const aggregate = await bundleEntry({
      root,
      entrySource: aggregateEntry(modules),
      sourcefile: "<helmr-analysis>",
      outfile: resolve5(root, "helmr/analysis.mjs"),
      nodeVersion: options.nodeVersion,
      runtimeRoot: root,
      localPackages: canonicalLocalPackages
    });
    await writeFile(aggregatePath, aggregate.code);
    const namespaces = await importAggregate(aggregatePath);
    const analyzed = analyze({
      architecture: options.architecture,
      exports: analysisExports(namespaces)
    });
    const canonicalSources = [
      ...new Set(analyzed.declarationLocator.declarations.map((item) => item.modulePath))
    ].sort(compareUTF82);
    const generated = new Map;
    const files = new Map;
    const finalOutputs = [];
    const localPackageGroups = [
      aggregate.localPackages
    ];
    const metafiles = [aggregate.metafile];
    const externalEdgeGroups = [];
    const runtimeRoot = resolve5(options.runtimeRoot ?? RUNTIME_PROGRAM_ROOT);
    for (const source2 of canonicalSources) {
      const selected = analyzed.declarationLocator.declarations.filter((item) => item.modulePath === source2);
      const path = generatedModulePath(source2);
      const compiled = await bundleEntry({
        root,
        entrySource: finalEntry(source2, selected.map((item) => item.exportName)),
        sourcefile: `<helmr-final-${source2}>`,
        outfile: resolve5(root, path),
        nodeVersion: options.nodeVersion,
        runtimeRoot,
        localPackages: canonicalLocalPackages
      });
      const sourceMap = normalizeSourceMap(root, resolve5(root, path), source2, compiled.map, compiled.localPackages);
      const analysisModulePath = resolve5(analysisRoot, path);
      await mkdir(dirname3(analysisModulePath), { recursive: true });
      await writeFile(analysisModulePath, compiled.code);
      await writeFile(`${analysisModulePath}.map`, sourceMap);
      await verifyFinalModule(analysisModulePath, source2, analyzed, options.architecture);
      generated.set(source2, path);
      files.set(path, compiled.code);
      files.set(`${path}.map`, sourceMap);
      metafiles.push(compiled.metafile);
      localPackageGroups.push(compiled.localPackages);
      externalEdgeGroups.push(compiled.externalEdges);
      finalOutputs.push({
        moduleDigest: `sha256:${sha256(compiled.code)}`,
        modulePath: path,
        sourceMapDigest: `sha256:${sha256(sourceMap)}`,
        sourceMapPath: `${path}.map`,
        sourcePath: source2
      });
    }
    finalOutputs.sort((left, right) => compareUTF82(left.modulePath, right.modulePath));
    const locator = {
      declarations: analyzed.declarationLocator.declarations.map((item) => ({
        ...item,
        modulePath: requiredGeneratedPath(generated, item.modulePath)
      })),
      formatVersion: analyzed.declarationLocator.formatVersion
    };
    const locatorBytes = canonicalizeJsonValue(locator);
    const analysis = Object.freeze({
      ...analyzed,
      declarationLocator: Object.freeze({
        ...locator,
        declarations: Object.freeze(locator.declarations)
      }),
      declarationLocatorBytes: locatorBytes
    });
    const configBytes = canonicalizeJsonValue(options.config);
    files.set("helmr/config.json", configBytes);
    const localPackages = mergeLocalPackages(localPackageGroups);
    const externalEdges = mergeExternalEdges(externalEdgeGroups);
    const inputs = await compilerInputs(root, metafiles, localPackages);
    const tsconfigs = await compilerTSConfigs(root, inputs.map((item) => item.path));
    files.set("helmr/compiler-result.json", canonicalizeJsonValue({
      compiler: compilerContract(),
      config: {
        digest: `sha256:${sha256(configBytes)}`,
        path: "helmr/config.json"
      },
      execution: {
        nodeVersion: options.nodeVersion,
        optionsDigest: compilerOptionsDigest(options.nodeVersion)
      },
      discoveryCandidates: modules,
      externalEdges,
      inputs,
      localPackages,
      outputs: finalOutputs,
      selections: analyzed.declarationLocator.declarations.map((item) => ({
        declaredId: item.declaredId,
        exportName: item.exportName,
        kind: item.kind,
        slot: item.slot,
        sourcePath: item.modulePath
      })),
      tsconfigs
    }));
    return Object.freeze({
      analysis,
      files,
      modules: Object.freeze(modules),
      optionsDigest: compilerOptionsDigest(options.nodeVersion)
    });
  } finally {
    await rm(analysisRoot, { force: true, recursive: true });
  }
}
function compilerOptionsContractDigest() {
  return compilerOptionsDigestForTarget("exact-managed-node");
}
function compilerOptionsDigest(nodeVersion) {
  return compilerOptionsDigestForTarget(esbuildNodeTarget(nodeVersion));
}
function compilerOptionsDigestForTarget(target) {
  const canonical = canonicalizeJsonValue({
    apiVersion: COMPILER_API_VERSION,
    banner: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);',
    bundle: true,
    esbuildVersion: ESBUILD_VERSION,
    format: "esm",
    legalComments: "none",
    metafile: true,
    packages: "bundle",
    platform: "node",
    preserveSymlinks: false,
    sourceMap: "external",
    sourceMapSources: "absolute-program-urls",
    sourcesContent: false,
    splitting: false,
    treeShaking: true,
    declarationExtensions,
    sourceSemantics: "pinned-esbuild",
    target,
    write: false
  });
  return `sha256:${sha256(canonical)}`;
}
async function bundleEntry(options) {
  const localPackages = new Map(options.localPackages.map((item) => [item.installedRoot, item]));
  const externalEdges = [];
  const result = await build2({
    ...baseOptions(options.root, options.outfile, options.runtimeRoot, options.nodeVersion, localPackages, externalEdges),
    stdin: {
      contents: options.entrySource,
      loader: "js",
      resolveDir: options.root,
      sourcefile: options.sourcefile
    }
  });
  const output = {
    ...outputFiles(requireOutputFiles(result.outputFiles), options.outfile),
    localPackages: sortedLocalPackages(localPackages),
    externalEdges: sortedExternalEdges(externalEdges),
    metafile: requiredMetafile(result.metafile)
  };
  return output;
}
function baseOptions(root, outfile, runtimeRoot, nodeVersion, localPackages, externalEdges) {
  return {
    absWorkingDir: root,
    bundle: true,
    format: "esm",
    legalComments: "none",
    logLevel: "silent",
    metafile: true,
    outfile,
    packages: "bundle",
    platform: "node",
    plugins: [dependencyBoundary(root, runtimeRoot, localPackages, externalEdges)],
    banner: {
      js: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);'
    },
    sourcesContent: false,
    sourcemap: "external",
    splitting: false,
    target: esbuildNodeTarget(nodeVersion),
    treeShaking: true,
    write: false
  };
}
async function verifyFinalModule(path, source2, aggregate, architecture) {
  const namespace = await importModuleNamespace(path);
  const expected = aggregate.declarationLocator.declarations.filter((item) => item.modulePath === source2);
  const exports = expected.map((item) => {
    if (!Object.prototype.hasOwnProperty.call(namespace, item.exportName)) {
      throw new Error(`final module ${JSON.stringify(source2)} is missing export ${JSON.stringify(item.exportName)}`);
    }
    return {
      exportName: item.exportName,
      modulePath: source2,
      value: namespace[item.exportName]
    };
  });
  const verified = analyzeProgramExports({ architecture, exports });
  const actual = canonicalizeJsonValue({
    declarations: verified.declarationLocator.declarations,
    programDeclarations: verified.programDeclarations
  });
  const wanted = canonicalizeJsonValue({
    declarations: expected,
    programDeclarations: aggregate.programDeclarations.filter((item) => expected.some((located) => located.kind === item.kind && located.declaredId === item.declaredId))
  });
  if (!Buffer.from(actual).equals(Buffer.from(wanted))) {
    throw new Error(`final module ${JSON.stringify(source2)} does not match aggregate analysis`);
  }
}
async function importModuleNamespace(path) {
  const value = await import(`${pathToFileURL(path).href}?digest=${sha256(await readFile2(path))}`);
  if (typeof value !== "object" || value === null) {
    throw new Error("generated final module has no ESM namespace");
  }
  return value;
}
function esbuildNodeTarget(nodeVersion) {
  if (!/^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/.test(nodeVersion)) {
    throw new Error("Compiler Node version must be an exact canonical SemVer");
  }
  return `node${nodeVersion}`;
}
function dependencyBoundary(root, runtimeRoot, localPackages, externalEdges) {
  const canonicalRoot = resolve5(root);
  return {
    name: "helmr-dependency-boundary",
    setup(build3) {
      build3.onResolve({ filter: /.*/ }, async (args) => {
        if (args.pluginData === resolvedByBoundary || args.path.startsWith("node:")) {
          return;
        }
        const result = await build3.resolve(args.path, {
          importer: args.importer,
          kind: args.kind,
          namespace: args.namespace,
          pluginData: resolvedByBoundary,
          resolveDir: args.resolveDir,
          with: args.with
        });
        if (result.errors.length !== 0 || result.external)
          return result;
        if (result.path === "")
          return result;
        const logicalPath = projectPath3(canonicalRoot, resolve5(result.path));
        const path = await realpath4(result.path);
        const resolvedPath = projectPath3(canonicalRoot, path);
        if (!inside3(relative4(canonicalRoot, path))) {
          return {
            errors: [{
              text: `resolved path escapes submitted source: ${args.path}`
            }]
          };
        }
        if (localPackageForPath(logicalPath, localPackages) !== undefined || localPackageForPath(resolvedPath, localPackages) !== undefined) {
          return { path };
        }
        if (hasNodeModules2(resolvedPath)) {
          const runtimePath = resolve5(runtimeRoot, logicalPath);
          externalEdges.push({
            importer: args.importer === "" ? args.importer : projectPath3(canonicalRoot, resolve5(args.importer)),
            kind: args.kind,
            logicalPath,
            resolvedPath,
            runtimePath,
            specifier: args.path
          });
          const target = resolve5(runtimeRoot, logicalPath);
          return {
            external: true,
            path: target
          };
        }
        return { path };
      });
    }
  };
}
var resolvedByBoundary = Object.freeze({});
function localPackageForPath(path, localPackages) {
  for (const localPackage of localPackages.values()) {
    if (path === localPackage.installedRoot || path.startsWith(`${localPackage.installedRoot}/`)) {
      return localPackage;
    }
  }
  return;
}
function sortedLocalPackages(localPackages) {
  return [...localPackages.values()].sort((left, right) => compareUTF82(left.installedRoot, right.installedRoot));
}
function mergeLocalPackages(groups) {
  const merged = new Map;
  for (const group of groups) {
    for (const localPackage of group) {
      const previous = merged.get(localPackage.installedRoot);
      if (previous !== undefined && (previous.name !== localPackage.name || previous.sourceRoot !== localPackage.sourceRoot)) {
        throw new Error(`installed local package ${JSON.stringify(localPackage.installedRoot)} changed classification`);
      }
      merged.set(localPackage.installedRoot, localPackage);
    }
  }
  return sortedLocalPackages(merged);
}
function sortedExternalEdges(edges) {
  return [...edges].sort((left, right) => compareUTF82(externalEdgeKey(left), externalEdgeKey(right)));
}
function mergeExternalEdges(groups) {
  const merged = new Map;
  for (const edge of groups.flat()) {
    merged.set(externalEdgeKey(edge), edge);
  }
  return sortedExternalEdges([...merged.values()]);
}
function externalEdgeKey(edge) {
  return [
    edge.importer,
    edge.specifier,
    edge.kind,
    edge.logicalPath,
    edge.resolvedPath,
    edge.runtimePath
  ].join("\x00");
}
function aggregateEntry(modules) {
  const imports = modules.map((path, index) => `import * as module${index} from ${JSON.stringify(`./${path}`)};`);
  const entries = modules.map((path, index) => `{ modulePath: ${JSON.stringify(path)}, namespace: module${index} }`);
  return `${imports.join(`
`)}
export default [${entries.join(",")}];
`;
}
function finalEntry(source2, exportNames) {
  const bindings = exportNames.map((name, index) => `const helmrExport${index} = source[${JSON.stringify(name)}];`);
  const exports = exportNames.map((name, index) => `helmrExport${index} as ${JSON.stringify(name)}`);
  return [
    `import * as source from ${JSON.stringify(`./${source2}`)};`,
    ...bindings,
    `export { ${exports.join(", ")} };`,
    ""
  ].join(`
`);
}
function generatedModulePath(source2) {
  const directory = dirname3(source2);
  const prefix = directory === "." ? "" : `${directory}/`;
  return `${prefix}.helmr/modules/${sha256(source2)}.mjs`;
}
async function importAggregate(path) {
  const value = await import(`${pathToFileURL(path).href}?digest=${sha256(await readFile2(path))}`);
  if (typeof value !== "object" || value === null || !Array.isArray(value["default"])) {
    throw new Error("analysis bundle did not export module namespaces");
  }
  return value.default;
}
function analysisExports(modules) {
  return modules.flatMap(({ modulePath, namespace }) => Object.getOwnPropertyNames(namespace).sort(compareUTF82).map((exportName) => ({
    exportName,
    modulePath,
    value: namespace[exportName]
  })));
}
function normalizeSourceMap(root, outfile, source2, raw, localPackages) {
  const value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw));
  if (typeof value !== "object" || value === null) {
    throw new Error("esbuild source map root is not an object");
  }
  const map = value;
  if (map["version"] !== 3 || !Array.isArray(map["sources"]) || !map["sources"].every((item) => typeof item === "string") || typeof map["mappings"] !== "string" || !Array.isArray(map["names"])) {
    throw new Error("esbuild source map does not match the v0 topology");
  }
  const sources = map["sources"].map((item) => {
    if (item.includes("%3Chelmr-final-") || item.includes("<helmr-final-")) {
      return programSourceURL(source2);
    }
    const decoded = decodeURIComponent(item);
    const absolute = resolve5(dirname3(outfile), decoded);
    const path = projectPath3(root, absolute);
    if (!inside3(relative4(root, absolute)) || hasNodeModules2(path) && localPackageForPath(path, new Map(localPackages.map((item2) => [item2.installedRoot, item2]))) === undefined) {
      throw new Error(`source map source escapes first-party Program files: ${item}`);
    }
    return programSourceURL(path);
  });
  return canonicalizeJsonValue({
    mappings: map["mappings"],
    names: map["names"],
    sources,
    version: 3
  });
}
function programSourceURL(path) {
  return pathToFileURL(resolve5(RUNTIME_PROGRAM_ROOT, path)).href;
}
function outputFiles(files, outfile) {
  const code = files.find((file) => file.path === outfile);
  const map = files.find((file) => file.path === `${outfile}.map`);
  if (code === undefined || map === undefined || files.length !== 2) {
    throw new Error("esbuild output topology does not match the v0 contract");
  }
  return { code: code.contents, map: map.contents };
}
function requireOutputFiles(files) {
  if (files === undefined)
    throw new Error("esbuild returned no output files");
  return files;
}
function requiredMetafile(metafile) {
  if (metafile === undefined)
    throw new Error("esbuild returned no metafile");
  return metafile;
}
async function compilerInputs(root, metafiles, localPackages) {
  const paths = new Set;
  for (const metafile of metafiles) {
    for (const input of Object.keys(metafile.inputs)) {
      if (input.startsWith("<"))
        continue;
      const absolute = await realpath4(resolve5(root, input));
      const path = projectPath3(root, absolute);
      if (!inside3(relative4(root, absolute)))
        continue;
      if (hasNodeModules2(path) && !localPackages.some((item) => path === item.installedRoot || path.startsWith(`${item.installedRoot}/`))) {
        continue;
      }
      paths.add(path);
    }
  }
  const sorted = [...paths].sort(compareUTF82);
  return Promise.all(sorted.map(async (path) => ({
    digest: `sha256:${sha256(await readFile2(resolve5(root, path)))}`,
    path
  })));
}
async function compilerTSConfigs(root, inputs) {
  const roots = new Set;
  for (const input of inputs) {
    let directory = dirname3(resolve5(root, input));
    for (;; ) {
      const candidate = resolve5(directory, "tsconfig.json");
      try {
        await readFile2(candidate);
        roots.add(candidate);
        break;
      } catch (error) {
        if (error.code !== "ENOENT")
          throw error;
      }
      if (directory === root)
        break;
      const parent = dirname3(directory);
      if (!inside3(relative4(root, parent)))
        break;
      directory = parent;
    }
  }
  const configs = new Map;
  const pending = [...roots];
  while (pending.length !== 0) {
    const candidate = await realpath4(pending.shift());
    const path = projectPath3(root, candidate);
    if (!inside3(path)) {
      throw new Error(`tsconfig path escapes project: ${candidate}`);
    }
    if (configs.has(path))
      continue;
    const contents = await readFile2(candidate);
    configs.set(path, `sha256:${sha256(contents)}`);
    const errors = [];
    const document = parse2(contents.toString("utf8"), errors, {
      allowTrailingComma: true,
      disallowComments: false
    });
    if (errors.length !== 0 || typeof document !== "object" || document === null || Array.isArray(document)) {
      throw new Error(`tsconfig ${JSON.stringify(path)} is not valid JSONC`);
    }
    const extended = document["extends"];
    const values = typeof extended === "string" ? [extended] : Array.isArray(extended) && extended.every((value) => typeof value === "string") ? extended : extended === undefined ? [] : (() => {
      throw new Error(`tsconfig ${JSON.stringify(path)} has invalid extends`);
    })();
    for (const specifier of values) {
      pending.push(await resolveTSConfigExtends(candidate, specifier));
    }
  }
  return [...configs].map(([path, digest]) => ({ digest, path })).sort((left, right) => compareUTF82(left.path, right.path));
}
async function resolveTSConfigExtends(configPath, specifier) {
  const directory = dirname3(configPath);
  if (specifier.startsWith(".") || specifier.startsWith("/") || /^[A-Za-z]:[\\/]/.test(specifier)) {
    return requiredConfigPath(resolve5(directory, specifier));
  }
  const require2 = createRequire2(pathToFileURL(configPath));
  for (const candidate of [specifier, `${specifier}/tsconfig.json`]) {
    try {
      return require2.resolve(candidate);
    } catch (error) {
      if (error.code !== "MODULE_NOT_FOUND") {
        throw error;
      }
    }
  }
  throw new Error(`tsconfig ${JSON.stringify(configPath)} cannot resolve extends ${JSON.stringify(specifier)}`);
}
async function requiredConfigPath(candidate) {
  for (const path of [
    candidate,
    `${candidate}.json`,
    resolve5(candidate, "tsconfig.json")
  ]) {
    try {
      if ((await stat3(path)).isFile())
        return path;
    } catch (error) {
      if (error.code !== "ENOENT")
        throw error;
    }
  }
  throw new Error(`extended tsconfig ${JSON.stringify(candidate)} does not exist`);
}
function projectPath3(root, value) {
  return relative4(root, value).split(sep4).join("/");
}
function requiredGeneratedPath(generated, source2) {
  const path = generated.get(source2);
  if (path === undefined)
    throw new Error(`missing final module for ${source2}`);
  return path;
}
function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}
function hasNodeModules2(path) {
  return path.split(sep4).includes("node_modules");
}
function inside3(path) {
  return path === "" || path !== ".." && !path.startsWith(`..${sep4}`) && !path.startsWith("/");
}

// compiler/typescript/src/config.ts
function inspectCanonicalConfig(value) {
  if (typeof value !== "object" || value === null || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) {
    throw new Error("canonical config must be an ordinary object");
  }
  const record = value;
  const keys = Object.keys(record).sort();
  if (keys.length !== 2 || keys[0] !== "dirs" || keys[1] !== "ignorePatterns") {
    throw new Error("canonical config does not match the build contract");
  }
  return inspectConfig({
    dirs: record["dirs"],
    ignorePatterns: record["ignorePatterns"]
  });
}

// compiler/typescript/src/program-compiler.ts
async function main() {
  if (process.argv.length === 3 && process.argv[2] === "--describe") {
    process.stdout.write(canonicalizeJsonValue(compilerContract()));
    return;
  }
  if (process.argv.length !== 6 || process.argv[2] === undefined || process.argv[3] === undefined || process.argv[4] === undefined || process.argv[5] === undefined) {
    throw new Error("Program Compiler requires a Program root, canonical config path, exact Node version, and output root");
  }
  const root = resolve6(process.argv[2]);
  const config = inspectCanonicalConfig(JSON.parse(await readFile3(process.argv[3], "utf8")));
  const compiled = await compileProgram({
    architecture: "x86_64",
    config,
    nodeVersion: process.argv[4],
    outputRoot: process.argv[5],
    root
  });
  for (const [path, contents] of compiled.files) {
    const target = resolve6(process.argv[5], path);
    await mkdir2(dirname4(target), { recursive: true });
    await writeFile2(target, contents);
  }
  await writeResult(successfulVerificationResult(compiled.analysis));
}
async function writeResult(result) {
  const configured = process.env["HELMR_SUPERVISOR_FD"];
  const fd = configured === undefined ? 3 : Number(configured);
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("Program Compiler result descriptor is invalid");
  }
  const output = createWriteStream("", { fd, autoClose: false });
  const frame = encodeVerificationResultFrame(result);
  await new Promise((resolve7, reject) => {
    output.once("error", reject);
    output.end(frame, resolve7);
  });
}
try {
  await main();
} catch (error) {
  if (process.argv[2] === "--describe")
    throw error;
  const message = error instanceof Error ? error.message : String(error);
  await writeResult(failedVerificationResult(message));
}
