package runtime

import (
	"strings"
	"github.com/revature/corems-code-executor/internal/models"
)

const sandboxBinary = "/sandbox/main"

// BuildCompileCommand returns the compile command args for the given language
// and source file path.  It returns nil when the language does not require a
// separate compilation step (interpreted languages).
func BuildCompileCommand(lang *models.Language, sourceFile string) []string {
	if lang.CompileCommand == nil || *lang.CompileCommand == "" {
		return nil
	}

	// The compile command stored in the database uses the bare filename; we
	// substitute it with the full /sandbox-rooted path.
	raw := *lang.CompileCommand
	return tokenise(raw)
}

// BuildRunCommand returns the run command args for the given language, source
// file, and optional extra command-line arguments string.
func BuildRunCommand(lang *models.Language, sourceFile, args string) []string {
	cmd := lang.RunCommand
	tokens := tokenise(cmd)

	// Append extra command-line arguments if provided.
	if args != "" {
		tokens = append(tokens, tokenise(args)...)
	}

	return tokens
}

// tokenise splits a shell-style command string into tokens.  It honours
// double-quoted strings but intentionally does not support single quotes or
// escape sequences — the stored commands are simple enough not to need them.
func tokenise(cmd string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false

	for _, r := range cmd {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// ─── Language-specific overrides ─────────────────────────────────────────────
// The functions below provide explicit, well-tested compile/run commands for
// languages where the stored database value needs to be constructed dynamically
// (e.g., because the output binary name is derived from the source file).
// They are used by BuildExecutionRequest instead of the generic tokeniser.

// compileC returns gcc compile args for a C source file.
func compileC(sourceFile string) []string {
	return []string{
		"gcc", "-O2", "-o", sandboxBinary,
		sourceFile, "-lm",
	}
}

// compileCPP returns g++ compile args for a C++ source file.
func compileCPP(sourceFile string) []string {
	return []string{
		"g++", "-O2", "-std=c++17", "-o", sandboxBinary,
		sourceFile, "-lm",
	}
}

// compileRust returns rustc compile args.
func compileRust(sourceFile string) []string {
	return []string{
		"rustc", "-o", sandboxBinary, sourceFile,
	}
}

// compileJava returns javac compile args.
func compileJava(sourceFile string) []string {
	return []string{
		"javac", "-d", "/sandbox", sourceFile,
	}
}

// compileGo returns go build compile args.
func compileGo(sourceFile string) []string {
	return []string{
		"go", "build", "-o", sandboxBinary, sourceFile,
	}
}

// compileKotlin returns kotlinc compile args.
func compileKotlin(sourceFile string) []string {
	return []string{
		"kotlinc", sourceFile, "-include-runtime", "-d", "/sandbox/solution.jar",
	}
}

// compileCSharp returns mcs/dotnet compile args for C#.
func compileCSharp(sourceFile string) []string {
	return []string{
		"mcs", sourceFile, "-out:/sandbox/solution.exe",
	}
}

// compileHaskell returns ghc compile args.
func compileHaskell(sourceFile string) []string {
	return []string{
		"ghc", "-o", sandboxBinary, sourceFile,
	}
}

// compileSwift returns swiftc compile args.
func compileSwift(sourceFile string) []string {
	return []string{
		"swiftc", sourceFile, "-o", sandboxBinary,
	}
}

// compileScala returns nil because we run Scala in script mode
// (`scala <file>.scala`), which compiles and runs in one step and handles
// every entry-point style — top-level `@main def NAME`, classic
// `object Main { def main(args) = ... }`, and bare top-level expressions.
// The previous two-step `scalac` + `scala -cp /sandbox Main` flow only
// worked for entry points compiled to a class literally named Main.
func compileScala(sourceFile string) []string {
	return nil
}

// runJava returns args for running a compiled Java class.
func runJava(className string) []string {
	return []string{
		"java", "-cp", "/sandbox", className,
	}
}

// runKotlin returns args for running a compiled Kotlin jar.
func runKotlin() []string {
	return []string{
		"java", "-jar", "/sandbox/solution.jar",
	}
}

// runCSharp returns args for running a compiled C# exe via mono.
func runCSharp() []string {
	return []string{
		"mono", "/sandbox/solution.exe",
	}
}

// runScala runs the source file in scala-cli / Scala 3 script mode. The
// launcher detects the entry point automatically: `@main def NAME`,
// `object Main { def main(args) = ... }`, or a top-level script.
func runScala(sourceFile string) []string {
	return []string{"scala", sourceFile}
}

// javaClassName extracts the public class name from a Java source filename.
// e.g. "Main.java" → "Main", "Solution.java" → "Solution".
func javaClassName(sourceFile string) string {
	base := sourceFile
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	return strings.TrimSuffix(base, ".java")
}

// scalaObjectName is no longer used — Scala now runs in script mode via
// `scala <file>.scala`. Kept removed intentionally.
