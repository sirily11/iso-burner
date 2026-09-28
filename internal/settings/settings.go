// Package settings holds the user-chosen ISO creation settings and the
// helpers that derive output names and chunk layouts from them.
package settings

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SizePreset is a named target capacity for a single ISO chunk.
type SizePreset struct {
	Name  string
	Bytes int64
}

// Presets are the supported target ISO sizes. Each target is about 2% below
// the disc's exact user-data capacity (25,025,314,816 bytes for BD-25 and
// 50,050,629,632 for BD-50), leaving headroom for burners that reserve spare
// area. Sizes are decimal gigabytes, as shown by Finder.
var Presets = []SizePreset{
	{Name: "Blu-ray 25GB (BD-25)", Bytes: 24_500_000_000},
	{Name: "Blu-ray 50GB (BD-50)", Bytes: 49_000_000_000},
}

// Settings is the complete configuration collected by the TUI.
type Settings struct {
	Folder  string
	Pattern *regexp.Regexp
	Preset  SizePreset
	ISOName string
}

// invalidNameChars are characters that are unsafe in file names on common
// filesystems.
const invalidNameChars = `/\:*?"<>|`

// ValidateFolder checks that path exists and is a directory.
func ValidateFolder(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("folder is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot access folder: %w", err)
	}
	if !info.IsDir() {
		return errors.New("path is not a folder")
	}
	return nil
}

// CompilePattern compiles the file-selection pattern. An empty pattern
// matches every file. The pattern is tried as a regex first; if that fails
// it is interpreted as a glob (e.g. "**/*.mp4" or "*.mkv"). Patterns
// containing "/*" are always globs, since "zero or more slashes" is never
// the intended regex meaning.
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		pattern = ".*"
	}
	re, err := regexp.Compile(pattern)
	if err == nil && !strings.Contains(pattern, "/*") {
		return re, nil
	}
	if err == nil {
		err = errors.New("invalid glob")
	}
	if globRe, globErr := regexp.Compile(globToRegex(pattern)); globErr == nil {
		return globRe, nil
	}
	return nil, fmt.Errorf("invalid regex or glob: %w", err)
}

// globToRegex converts a glob to an anchored regex matched against
// slash-separated relative paths. "*" and "?" do not cross "/", "**/" matches
// zero or more directories, and "[...]" classes are passed through. A glob
// without "/" matches the base name at any depth, like .gitignore.
func globToRegex(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	if !strings.Contains(glob, "/") {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// ValidateISOName checks that name can be used as the output file prefix.
func ValidateISOName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("ISO name is required")
	}
	if strings.ContainsAny(name, invalidNameChars) {
		return fmt.Errorf("ISO name must not contain any of %s", invalidNameChars)
	}
	return nil
}

// OutputName returns the file name for a chunk: {iso_name}_{chunk_index}.iso.
// Chunk indexes are 1-based.
func OutputName(isoName string, chunkIndex int) string {
	return fmt.Sprintf("%s_%d.iso", isoName, chunkIndex)
}

// FormatBytes renders a byte count using binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// FormatBytesDecimal renders a byte count using decimal (SI) units, matching
// how Finder and disc labels report sizes.
func FormatBytesDecimal(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
