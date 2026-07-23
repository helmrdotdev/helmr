package archive

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

const maxSourceIgnoreBytes = 1 << 20

type gitIgnore struct {
	patterns []gitIgnorePattern
}

type SourceIgnore struct {
	ignore *gitIgnore
}

type gitIgnorePattern struct {
	exclude bool
	dirOnly bool
	path    bool
	match   string
}

func parseSourceIgnore(body []byte) (*gitIgnore, error) {
	ignore := &gitIgnore{}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 64*1024), maxSourceIgnoreBytes+1)
	for scanner.Scan() {
		pattern, ok := parseGitIgnorePattern(scanner.Text())
		if ok {
			ignore.patterns = append(ignore.patterns, pattern)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read .helmrignore: %w", err)
	}
	return ignore, nil
}

func ParseSourceIgnore(body []byte) (SourceIgnore, error) {
	ignore, err := parseSourceIgnore(body)
	if err != nil {
		return SourceIgnore{}, err
	}
	return SourceIgnore{ignore: ignore}, nil
}

func (ignore SourceIgnore) Match(name string, isDir bool) bool {
	if ignore.ignore == nil {
		return false
	}
	return ignore.ignore.Match(name, isDir)
}

func (ignore *gitIgnore) Match(name string, isDir bool) bool {
	excluded := false
	for _, pattern := range ignore.patterns {
		target := name
		if !pattern.path {
			target = pathBase(name)
		}
		if pattern.dirOnly && !isDir {
			continue
		}
		if wildMatch(pattern.match, target) {
			excluded = pattern.exclude
		}
	}
	return excluded
}

func pathBase(name string) string {
	if index := strings.LastIndexByte(name, '/'); index >= 0 {
		return name[index+1:]
	}
	return name
}

func parseGitIgnorePattern(line string) (gitIgnorePattern, bool) {
	line = strings.TrimSuffix(line, "\r")
	line = trimUnescapedSpaces(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return gitIgnorePattern{}, false
	}
	exclude := true
	if strings.HasPrefix(line, "!") {
		exclude = false
		line = line[1:]
	} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if line == "" {
		return gitIgnorePattern{}, false
	}
	dirOnly := strings.HasSuffix(line, "/") && !strings.HasSuffix(line, `\/`)
	if dirOnly {
		line = strings.TrimSuffix(line, "/")
	}
	anchored := strings.HasPrefix(line, "/")
	line = strings.TrimPrefix(line, "/")
	return gitIgnorePattern{
		exclude: exclude,
		dirOnly: dirOnly,
		path:    anchored || strings.Contains(line, "/"),
		match:   line,
	}, true
}

func trimUnescapedSpaces(value string) string {
	end := len(value)
	for end > 0 && value[end-1] == ' ' {
		backslashes := 0
		for index := end - 2; index >= 0 && value[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		end--
	}
	return value[:end]
}

type wildState struct {
	pattern int
	text    int
}

func wildMatch(pattern, text string) bool {
	memo := make(map[wildState]bool)
	known := make(map[wildState]bool)
	var match func(int, int) bool
	match = func(patternIndex, textIndex int) bool {
		state := wildState{pattern: patternIndex, text: textIndex}
		if known[state] {
			return memo[state]
		}
		known[state] = true
		result := matchWild(pattern, text, patternIndex, textIndex, match)
		memo[state] = result
		return result
	}
	return match(0, 0)
}

func matchWild(
	pattern string,
	text string,
	patternIndex int,
	textIndex int,
	match func(int, int) bool,
) bool {
	for patternIndex < len(pattern) {
		patternByte := pattern[patternIndex]
		switch patternByte {
		case '\\':
			patternIndex++
			if patternIndex == len(pattern) || textIndex == len(text) ||
				pattern[patternIndex] != text[textIndex] {
				return false
			}
			patternIndex++
			textIndex++
		case '?':
			if textIndex == len(text) || text[textIndex] == '/' {
				return false
			}
			patternIndex++
			textIndex++
		case '*':
			runEnd := patternIndex + 1
			for runEnd < len(pattern) && pattern[runEnd] == '*' {
				runEnd++
			}
			matchSlash := runEnd-patternIndex >= 2 &&
				(patternIndex == 0 || pattern[patternIndex-1] == '/') &&
				(runEnd == len(pattern) ||
					pattern[runEnd] == '/' ||
					(runEnd+1 < len(pattern) &&
						pattern[runEnd] == '\\' &&
						pattern[runEnd+1] == '/'))
			if matchSlash && runEnd < len(pattern) && pattern[runEnd] == '/' &&
				match(runEnd+1, textIndex) {
				return true
			}
			for candidate := textIndex; ; candidate++ {
				if match(runEnd, candidate) {
					return true
				}
				if candidate == len(text) || (!matchSlash && text[candidate] == '/') {
					return false
				}
			}
		case '[':
			if textIndex == len(text) || text[textIndex] == '/' {
				return false
			}
			classMatch, next, valid := matchWildClass(pattern, patternIndex, text[textIndex])
			if !valid || !classMatch {
				return false
			}
			patternIndex = next
			textIndex++
		default:
			if textIndex == len(text) || patternByte != text[textIndex] {
				return false
			}
			patternIndex++
			textIndex++
		}
	}
	return textIndex == len(text)
}

func matchWildClass(pattern string, start int, value byte) (bool, int, bool) {
	index := start + 1
	if index == len(pattern) {
		return false, start, false
	}
	negated := pattern[index] == '!' || pattern[index] == '^'
	if negated {
		index++
	}
	previous := byte(0)
	matched := false
	first := true
	for {
		if index == len(pattern) {
			return false, start, false
		}
		current := pattern[index]
		if current == ']' && !first {
			return matched != negated, index + 1, true
		}
		if current == '\\' {
			index++
			if index == len(pattern) {
				return false, start, false
			}
			current = pattern[index]
			if value == current {
				matched = true
			}
		} else if current == '-' && previous != 0 &&
			index+1 < len(pattern) && pattern[index+1] != ']' {
			index++
			current = pattern[index]
			if current == '\\' {
				index++
				if index == len(pattern) {
					return false, start, false
				}
				current = pattern[index]
			}
			if previous <= value && value <= current {
				matched = true
			}
			current = 0
		} else if current == '[' && index+1 < len(pattern) && pattern[index+1] == ':' {
			end := strings.Index(pattern[index+2:], ":]")
			if end < 0 {
				if value == '[' {
					matched = true
				}
			} else {
				class := pattern[index+2 : index+2+end]
				classMatch, valid := matchPOSIXClass(class, value)
				if !valid {
					return false, start, false
				}
				if classMatch {
					matched = true
				}
				index += end + 3
				current = 0
			}
		} else if value == current {
			matched = true
		}
		previous = current
		first = false
		index++
	}
}

func matchPOSIXClass(class string, value byte) (bool, bool) {
	isDigit := value >= '0' && value <= '9'
	isLower := value >= 'a' && value <= 'z'
	isUpper := value >= 'A' && value <= 'Z'
	isAlpha := isLower || isUpper
	isAlnum := isAlpha || isDigit
	isSpace := value == ' ' || value == '\t' || value == '\n' ||
		value == '\v' || value == '\f' || value == '\r'
	isPrint := value >= 0x20 && value <= 0x7e
	isGraph := value >= 0x21 && value <= 0x7e
	switch class {
	case "alnum":
		return isAlnum, true
	case "alpha":
		return isAlpha, true
	case "blank":
		return value == ' ' || value == '\t', true
	case "cntrl":
		return value < 0x20 || value == 0x7f, true
	case "digit":
		return isDigit, true
	case "graph":
		return isGraph, true
	case "lower":
		return isLower, true
	case "print":
		return isPrint, true
	case "punct":
		return isGraph && !isAlnum, true
	case "space":
		return isSpace, true
	case "upper":
		return isUpper, true
	case "xdigit":
		return isDigit || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F', true
	default:
		return false, false
	}
}

var errSourceChanged = errors.New("source changed during capture")
