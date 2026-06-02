package models

import "gorm.io/gorm"

// Docker image constants used across multiple language definitions.
// Locally-built images live under runtime-images/<lang>/ and are built by
// `make pull-images` (or `bash scripts/pull-images.sh`).
const (
	imageGCC   = "code-runtime-cpp:latest"    // shared by C, C++, Fortran
	imageNode  = "code-runtime-nodejs:latest" // shared by JavaScript, TypeScript
	srcFileSQL = "query.sql"
)

// Language ID constants matching Judge0 spec numbering.
const (
	LanguageBash           = 1
	LanguageC              = 2
	LanguageCPP            = 3
	LanguageCSharp         = 4
	LanguageClojure        = 5
	LanguageCOBOL          = 6
	LanguageCoffeeScript   = 7
	LanguageCrystal        = 8
	LanguageD              = 9
	LanguageElixir         = 10
	LanguageErlang         = 11
	LanguageFortran        = 12
	LanguageGo             = 13
	LanguageGroovy         = 14
	LanguageHaskell        = 15
	LanguageJava           = 16
	LanguageJavaScript     = 17
	LanguageKotlin         = 18
	LanguageLisp           = 19
	LanguageLua            = 20
	LanguageNim            = 21
	LanguageObjectiveC     = 22
	LanguageOCaml          = 23
	LanguagePascal         = 24
	LanguagePerl           = 25
	LanguagePHP            = 26
	LanguageProlog         = 27
	LanguagePython2        = 28
	LanguagePython3        = 29
	LanguageR              = 30
	LanguageRuby           = 31
	LanguageRust           = 32
	LanguageScala          = 33
	LanguageSwift          = 34
	LanguageTypeScript     = 35
	LanguageVBNet          = 36
	LanguageSQLite         = 37
	LanguageMySQL          = 38
	LanguagePostgreSQL     = 39
	LanguageMongoDB        = 40
)

// Language represents a supported programming language and its execution environment.
type Language struct {
	ID             int     `gorm:"primaryKey;autoIncrement"          json:"id"`
	Name           string  `gorm:"type:varchar(128);not null;unique"  json:"name"              validate:"required,max=128"`
	Version        string  `gorm:"type:varchar(64);not null"          json:"version"           validate:"required"`
	SourceFile     string  `gorm:"type:varchar(256);not null"         json:"source_file"       validate:"required"`
	CompileCommand *string `gorm:"type:varchar(1024)"                 json:"compile_command"`
	RunCommand     string  `gorm:"type:varchar(1024);not null"        json:"run_command"       validate:"required"`
	Image          string  `gorm:"type:varchar(256);not null"         json:"image"             validate:"required"`
	IsActive       bool    `gorm:"default:true"                       json:"is_active"`
	MaxMemory      int64   `gorm:"default:262144"                     json:"max_memory"`
	MaxCPUTime     float64 `gorm:"type:decimal(10,4);default:5.0"     json:"max_cpu_time"`
	IsDatabase     bool    `gorm:"default:false"                      json:"is_database"`
	DBType         string  `gorm:"type:varchar(64)"                   json:"db_type,omitempty"`
}

// TableName overrides the default GORM table name.
func (l *Language) TableName() string {
	return "languages"
}

// compilePtr is a helper to create a *string from a string literal.
func compilePtr(s string) *string {
	return &s
}

// DefaultLanguages returns the full set of supported languages with their Docker images and commands.
// Images use official public Docker Hub images that the worker pre-warms on startup.
// Languages without a readily available public image are marked IsActive: false.
func DefaultLanguages() []Language {
	return []Language{
		{
			ID:         LanguageBash,
			Name:       "Bash (5.2)",
			Version:    "5.2",
			SourceFile: "script.sh",
			RunCommand: "bash script.sh",
			Image:      "code-runtime-bash:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:             LanguageC,
			Name:           "C (GCC 13.3)",
			Version:        "13.3",
			SourceFile:     "main.c",
			CompileCommand: compilePtr("gcc main.c -o main -lm"),
			RunCommand:     "./main",
			Image:          imageGCC,
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     5.0,
		},
		{
			ID:             LanguageCPP,
			Name:           "C++ (GCC 13.3)",
			Version:        "13.3",
			SourceFile:     "main.cpp",
			CompileCommand: compilePtr("g++ main.cpp -o main -lm -std=c++17"),
			RunCommand:     "./main",
			Image:          imageGCC,
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     5.0,
		},
		{
			ID:         LanguageCSharp,
			Name:       "C# (Mono 6.12.0)",
			Version:    "6.12.0",
			SourceFile: "Main.cs",
			RunCommand: "mono Main.exe",
			Image:      "code-runtime-mono:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguageClojure,
			Name:       "Clojure (1.11.1)",
			Version:    "1.11.1",
			SourceFile: "main.clj",
			RunCommand: "clojure -M main.clj",
			Image:      "code-runtime-clojure:latest",
			IsActive:   true,
			MaxMemory:  524288, // 512 MB — JVM startup + Clojure runtime
			MaxCPUTime: 20.0,
		},
		{
			ID:             LanguageCOBOL,
			Name:           "COBOL (GnuCOBOL 3.1.2)",
			Version:        "3.1.2",
			SourceFile:     "main.cbl",
			CompileCommand: compilePtr("cobc -x -free -o /sandbox/main /sandbox/main.cbl"),
			RunCommand:     "/sandbox/main",
			Image:          "code-runtime-cobol:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:         LanguageCoffeeScript,
			Name:       "CoffeeScript (2.7.0)",
			Version:    "2.7.0",
			SourceFile: "main.coffee",
			RunCommand: "coffee main.coffee",
			Image:      "code-runtime-coffeescript:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:             LanguageCrystal,
			Name:           "Crystal (1.14)",
			Version:        "1.14",
			SourceFile:     "main.cr",
			CompileCommand: compilePtr("crystal build /sandbox/main.cr -o /sandbox/main"),
			RunCommand:     "/sandbox/main",
			Image:          "code-runtime-crystal:latest",
			IsActive:       true,
			MaxMemory:      1048576, // LLVM-based compiler needs up to ~1 GB
			MaxCPUTime:     30.0,
		},
		{
			ID:             LanguageD,
			Name:           "D (LDC2 1.33.0)",
			Version:        "1.33.0",
			SourceFile:     "main.d",
			CompileCommand: compilePtr("ldc2 -of=/sandbox/main /sandbox/main.d"),
			RunCommand:     "/sandbox/main",
			Image:          "code-runtime-d:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:         LanguageElixir,
			Name:       "Elixir (1.15.4)",
			Version:    "1.15.4",
			SourceFile: "main.exs",
			RunCommand: "elixir main.exs",
			Image:      "code-runtime-elixir:latest",
			IsActive:   true,
			MaxMemory:  524288,
			MaxCPUTime: 15.0,
		},
		{
			ID:             LanguageErlang,
			Name:           "Erlang (OTP 26.0)",
			Version:        "26.0",
			SourceFile:     "main.erl",
			CompileCommand: compilePtr("erlc main.erl"),
			RunCommand:     "erl -noshell -s main main -s init stop",
			Image:          "code-runtime-erlang:latest",
			IsActive:       true,
			MaxMemory:      524288,
			MaxCPUTime:     15.0,
		},
		{
			ID:             LanguageFortran,
			Name:           "Fortran (GFortran 13.3)",
			Version:        "13.3",
			SourceFile:     "main.f90",
			CompileCommand: compilePtr("gfortran main.f90 -o main"),
			RunCommand:     "./main",
			Image:          imageGCC,
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:             LanguageGo,
			Name:           "Go (1.24)",
			Version:        "1.24",
			SourceFile:     "main.go",
			CompileCommand: compilePtr("go build -o main main.go"),
			RunCommand:     "./main",
			Image:          "code-runtime-golang:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:         LanguageGroovy,
			Name:       "Groovy (4.0.15)",
			Version:    "4.0.15",
			SourceFile: "Main.groovy",
			RunCommand: "groovy /sandbox/Main.groovy",
			Image:      "code-runtime-groovy:latest",
			IsActive:   true,
			MaxMemory:  524288,
			MaxCPUTime: 15.0,
		},
		{
			ID:         LanguageHaskell,
			Name:       "Haskell (GHC 9.4.5)",
			Version:    "9.4.5",
			SourceFile: "Main.hs",
			RunCommand: "./main",
			Image:      "code-runtime-haskell:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 15.0,
		},
		{
			ID:             LanguageJava,
			Name:           "Java (OpenJDK 21)",
			Version:        "21",
			SourceFile:     "Main.java",
			CompileCommand: compilePtr("javac Main.java"),
			RunCommand:     "java Main",
			Image:          "code-runtime-java:latest",
			IsActive:       true,
			MaxMemory:      524288,
			MaxCPUTime:     15.0,
		},
		{
			ID:         LanguageJavaScript,
			Name:       "JavaScript (Node.js 22)",
			Version:    "22",
			SourceFile: "main.js",
			RunCommand: "node main.js",
			Image:      imageNode,
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguageKotlin,
			Name:       "Kotlin (2.3.21)",
			Version:    "2.3.21",
			SourceFile: "main.kt",
			RunCommand: "java -jar /sandbox/solution.jar",
			Image:      "code-runtime-kotlin:latest",
			IsActive:   true,
			MaxMemory:  2097152, // kotlinc JVM needs ~1–2 GB to compile
			MaxCPUTime: 60.0,
		},
		{
			ID:         LanguageLisp,
			Name:       "Common Lisp (SBCL 2.3.9)",
			Version:    "2.3.9",
			SourceFile: "main.lisp",
			RunCommand: "sbcl --script main.lisp",
			Image:      "code-runtime-lisp:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguageLua,
			Name:       "Lua (5.4)",
			Version:    "5.4",
			SourceFile: "main.lua",
			RunCommand: "lua main.lua",
			Image:      "code-runtime-lua:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:             LanguageNim,
			Name:           "Nim (2.0.2)",
			Version:        "2.0.2",
			SourceFile:     "main.nim",
			CompileCommand: compilePtr("nim c -o:/sandbox/main /sandbox/main.nim"),
			RunCommand:     "./main",
			Image:          "code-runtime-nim:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     15.0,
		},
		{
			ID:             LanguageObjectiveC,
			Name:           "Objective-C (GCC 11.4 / GNUstep)",
			Version:        "11.4",
			SourceFile:     "main.m",
			CompileCommand: compilePtr("sandbox-objc-compile /sandbox/main.m /sandbox/main"),
			RunCommand:     "/sandbox/main",
			Image:          "code-runtime-objc:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:             LanguageOCaml,
			Name:           "OCaml (5.4)",
			Version:        "5.4",
			SourceFile:     "main.ml",
			CompileCommand: compilePtr("ocamlopt -o /sandbox/main /sandbox/main.ml"),
			RunCommand:     "/sandbox/main",
			Image:          "code-runtime-ocaml:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:             LanguagePascal,
			Name:           "Pascal (FPC 3.2.2)",
			Version:        "3.2.2",
			SourceFile:     "main.pas",
			CompileCommand: compilePtr("fpc -o/sandbox/main /sandbox/main.pas"),
			RunCommand:     "/sandbox/main",
			Image:          "code-runtime-pascal:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:         LanguagePerl,
			Name:       "Perl (5.38)",
			Version:    "5.38",
			SourceFile: "main.pl",
			RunCommand: "perl main.pl",
			Image:      "code-runtime-perl:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguagePHP,
			Name:       "PHP (8.3)",
			Version:    "8.3",
			SourceFile: "main.php",
			RunCommand: "php main.php",
			Image:      "code-runtime-php:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguageProlog,
			Name:       "Prolog (SWI-Prolog 10.0.2)",
			Version:    "10.0.2",
			SourceFile: "main.pl",
			RunCommand: "swipl -q -f /sandbox/main.pl -g halt",
			Image:      "code-runtime-prolog:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguagePython2,
			Name:       "Python (2.7.18)",
			Version:    "2.7.18",
			SourceFile: "main.py",
			RunCommand: "python2 main.py",
			Image:      "code-runtime-python2:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguagePython3,
			Name:       "Python (3.12)",
			Version:    "3.12",
			SourceFile: "main.py",
			RunCommand: "python3 main.py",
			Image:      "code-runtime-python:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:         LanguageR,
			Name:       "R (4.3.2)",
			Version:    "4.3.2",
			SourceFile: "main.r",
			RunCommand: "Rscript main.r",
			Image:      "code-runtime-r:latest",
			IsActive:   true,
			MaxMemory:  524288,
			MaxCPUTime: 15.0,
		},
		{
			ID:         LanguageRuby,
			Name:       "Ruby (3.3)",
			Version:    "3.3",
			SourceFile: "main.rb",
			RunCommand: "ruby main.rb",
			Image:      "code-runtime-ruby:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
		},
		{
			ID:             LanguageRust,
			Name:           "Rust (1.82)",
			Version:        "1.82",
			SourceFile:     "main.rs",
			CompileCommand: compilePtr("rustc main.rs -o main"),
			RunCommand:     "./main",
			Image:          "code-runtime-rust:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     15.0,
		},
		{
			ID:         LanguageScala,
			Name:       "Scala (3.3.1)",
			Version:    "3.3.1",
			SourceFile: "Main.scala",
			// `scala <name>` with no extension treats <name> as a class name and
			// drops into the REPL when the class is missing. Pass the full path
			// so Scala 3 runs it as a script.
			RunCommand: "scala /sandbox/Main.scala",
			Image:      "code-runtime-scala:latest",
			IsActive:   true,
			MaxMemory:  524288,
			MaxCPUTime: 20.0,
		},
		{
			ID:         LanguageSwift,
			Name:       "Swift (5.9.2)",
			Version:    "5.9.2",
			SourceFile: "main.swift",
			RunCommand: "./main",
			Image:      "code-runtime-swift:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 15.0,
		},
		{
			ID:         LanguageTypeScript,
			Name:       "TypeScript (5.6.3)",
			Version:    "5.6.3",
			SourceFile: "main.ts",
			RunCommand: "npx tsx main.ts",
			Image:      "code-runtime-nodejs:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 15.0,
		},
		{
			ID:             LanguageVBNet,
			Name:           "Visual Basic.Net (Mono 6.12.0)",
			Version:        "6.12.0",
			SourceFile:     "Main.vb",
			CompileCommand: compilePtr("vbnc -out:/sandbox/Main.exe /sandbox/Main.vb"),
			RunCommand:     "mono /sandbox/Main.exe",
			Image:          "code-runtime-mono:latest",
			IsActive:       true,
			MaxMemory:      262144,
			MaxCPUTime:     10.0,
		},
		{
			ID:         LanguageSQLite,
			Name:       "SQLite (3.53.0)",
			Version:    "3.53.0",
			SourceFile: srcFileSQL,
			RunCommand: `sh -c "sqlite3 /tmp/db.sqlite < /sandbox/query.sql"`,
			Image:      "code-runtime-sqlite:latest",
			IsActive:   true,
			MaxMemory:  262144,
			MaxCPUTime: 10.0,
			IsDatabase: true,
			DBType:     "sqlite",
		},
		{
			ID:         LanguageMySQL,
			Name:       "MySQL (8.0)",
			Version:    "8.0",
			SourceFile: srcFileSQL,
			// RunCommand is not used; DBRuntime handles execution directly.
			RunCommand: "mysql --version",
			Image:      "code-runtime-mysql:latest",
			IsActive:   true,
			MaxMemory:  1048576, // 1 GB — mysqld init + server
			MaxCPUTime: 30.0,
			IsDatabase: true,
			DBType:     "mysql",
		},
		{
			ID:         LanguagePostgreSQL,
			Name:       "PostgreSQL (16)",
			Version:    "16",
			SourceFile: srcFileSQL,
			// RunCommand is not used; DBRuntime handles execution directly.
			RunCommand: "postgres --version",
			Image:      "code-runtime-postgresql:latest",
			IsActive:   true,
			MaxMemory:  1048576, // 1 GB — initdb + server
			MaxCPUTime: 30.0,
			IsDatabase: true,
			DBType:     "postgresql",
		},
		{
			ID:         LanguageMongoDB,
			Name:       "MongoDB (7.0)",
			Version:    "7.0",
			SourceFile: "solution.js",
			// RunCommand is not used; DBRuntime handles execution directly via mongosh.
			RunCommand: "mongosh --version",
			Image:      "code-runtime-mongodb:latest",
			IsActive:   true,
			MaxMemory:  1048576, // 1 GB — mongosh + embedded shell
			MaxCPUTime: 30.0,
			IsDatabase: true,
			DBType:     "mongodb",
		},
	}
}

// SeedLanguages upserts all default languages so that code changes to
// DefaultLanguages() take effect on the next API restart without a manual
// database migration.
const seedUpsertSQL = `
INSERT INTO languages
  (id, name, version, source_file, compile_command, run_command, image,
   is_active, max_memory, max_cpu_time, is_database, db_type)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  name            = EXCLUDED.name,
  version         = EXCLUDED.version,
  source_file     = EXCLUDED.source_file,
  compile_command = EXCLUDED.compile_command,
  run_command     = EXCLUDED.run_command,
  image           = EXCLUDED.image,
  is_active       = EXCLUDED.is_active,
  max_memory      = EXCLUDED.max_memory,
  max_cpu_time    = EXCLUDED.max_cpu_time,
  is_database     = EXCLUDED.is_database,
  db_type         = EXCLUDED.db_type`

func SeedLanguages(db *gorm.DB) error {
	for _, lang := range DefaultLanguages() {
		if err := db.Exec(seedUpsertSQL,
			lang.ID, lang.Name, lang.Version, lang.SourceFile,
			lang.CompileCommand, lang.RunCommand, lang.Image,
			lang.IsActive, lang.MaxMemory, lang.MaxCPUTime,
			lang.IsDatabase, lang.DBType,
		).Error; err != nil {
			return err
		}
	}
	return nil
}
