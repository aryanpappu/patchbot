package github

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// NeutralizeMentions inserts a zero-width joiner after @ in user and team
// mentions. Email addresses and Markdown inline code and fenced code are kept.
// Applying it more than once has no additional effect.
func NeutralizeMentions(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	var code byte
	var width int
	fenced := false
	lineStart := true
	indent := 0
	var previous rune
	for index := 0; index < len(text); {
		current := text[index]
		if current == '\n' {
			out.WriteByte(current)
			index++
			lineStart, indent, previous = true, 0, '\n'
			continue
		}
		if current == '`' || (current == '~' && lineStart) {
			end := index + 1
			for end < len(text) && text[end] == current {
				end++
			}
			run := end - index
			fence := lineStart && indent <= 3 && run >= 3
			if current == '`' {
				info, _, _ := strings.Cut(text[end:], "\n")
				fence = fence && !strings.ContainsRune(info, '`')
			}
			switch {
			case code == 0 && current == '`' && (!lineStart || indent <= 3) && !escapedAt(text, index) &&
				(fence || hasClosingTicks(text[end:], run)):
				code, width, fenced = current, run, fence
			case code == 0 && current == '~' && fence:
				code, width, fenced = current, run, true
			case code == current && ((!fenced && run == width) ||
				(fenced && lineStart && indent <= 3 && run >= width && fenceEnd(text[end:]))):
				code, width, fenced = 0, 0, false
			}
			out.WriteString(text[index:end])
			index, lineStart, previous = end, false, rune(current)
			continue
		}
		if current == '@' && code == 0 && index+1 < len(text) && mentionStart(text[index+1]) &&
			!emailAt(text, index, previous) {
			out.WriteString("@\u200d")
			index++
			lineStart, previous = false, '@'
			continue
		}
		character, size := utf8.DecodeRuneInString(text[index:])
		out.WriteString(text[index : index+size])
		index += size
		if current != ' ' && current != '\t' && current != '\r' {
			lineStart = false
		} else if lineStart {
			switch current {
			case '\t':
				indent += 4 - indent%4
			case ' ':
				indent++
			}
		}
		previous = character
	}
	return out.String()
}

func fenceEnd(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	return strings.Trim(line, " \t\r") == ""
}

func mentionStart(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func emailAt(text string, index int, previous rune) bool {
	if !unicode.IsLetter(previous) && !unicode.IsDigit(previous) && !strings.ContainsRune("._%+-", previous) {
		return false
	}
	end := index + 1
	for end < len(text) && (mentionStart(text[end]) || text[end] == '.' || text[end] == '-') {
		end++
	}
	domain := strings.TrimRight(text[index+1:end], ".")
	dot := strings.IndexByte(domain, '.')
	return dot > 0 && dot < len(domain)-1 && !strings.Contains(domain, "..")
}

func escapedAt(text string, index int) bool {
	count := 0
	for index > 0 && text[index-1] == '\\' {
		count++
		index--
	}
	return count%2 != 0
}

func hasClosingTicks(text string, width int) bool {
	for index := 0; index < len(text); {
		if text[index] != '`' {
			index++
			continue
		}
		end := index + 1
		for end < len(text) && text[end] == '`' {
			end++
		}
		if end-index == width {
			return true
		}
		index = end
	}
	return false
}
