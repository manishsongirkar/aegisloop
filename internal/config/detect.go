package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Detection describes what was found while scanning a repository.
type Detection struct {
	Root       string
	PHP        bool
	WordPress  bool
	JS         bool
	TypeScript bool
	Go         bool
	Scripts    map[string]string // package.json scripts
	Deps       map[string]bool   // package.json dependencies + devDependencies
	PHPDeps    map[string]bool   // composer.json require + require-dev
	PHPCSFile  string            // phpcs.xml / .phpcs.xml / phpcs.xml.dist when present
}

// Detect scans root for composer.json, package.json, phpcs rulesets and go.mod.
func Detect(root string) *Detection {
	d := &Detection{Root: root, Scripts: map[string]string{}, Deps: map[string]bool{}, PHPDeps: map[string]bool{}}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }

	for _, n := range []string{".phpcs.xml", "phpcs.xml", ".phpcs.xml.dist", "phpcs.xml.dist"} {
		if exists(n) {
			d.PHPCSFile = n
			d.PHP = true
			if b, err := os.ReadFile(filepath.Join(root, n)); err == nil && strings.Contains(strings.ToLower(string(b)), "wordpress") {
				d.WordPress = true
			}
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "composer.json")); err == nil {
		d.PHP = true
		var c struct {
			Require    map[string]any `json:"require"`
			RequireDev map[string]any `json:"require-dev"`
			Name       string         `json:"name"`
			Type       string         `json:"type"`
		}
		if json.Unmarshal(b, &c) == nil {
			for k := range c.Require {
				d.PHPDeps[k] = true
			}
			for k := range c.RequireDev {
				d.PHPDeps[k] = true
			}
			for k := range d.PHPDeps {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "wordpress") || strings.Contains(lk, "wpcs") || strings.Contains(lk, "vip-coding-standards") || strings.Contains(lk, "johnpbloch") {
					d.WordPress = true
				}
			}
			if strings.HasPrefix(c.Type, "wordpress-") {
				d.WordPress = true
			}
		}
	}
	if !d.PHP {
		// A bare PHP project without composer: look for a PHP file at depth ≤ 1.
		for _, pat := range []string{"*.php", "*/*.php"} {
			if m, _ := filepath.Glob(filepath.Join(root, pat)); len(m) > 0 {
				d.PHP = true
				break
			}
		}
	}
	if exists("wp-config.php") || exists("wp-content") || exists("functions.php") && exists("style.css") {
		d.WordPress, d.PHP = true, true
	}
	if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		d.JS = true
		var p struct {
			Scripts         map[string]string `json:"scripts"`
			Dependencies    map[string]any    `json:"dependencies"`
			DevDependencies map[string]any    `json:"devDependencies"`
		}
		if json.Unmarshal(b, &p) == nil {
			for k, v := range p.Scripts {
				d.Scripts[k] = v
			}
			for k := range p.Dependencies {
				d.Deps[k] = true
			}
			for k := range p.DevDependencies {
				d.Deps[k] = true
			}
		}
		if d.Deps["typescript"] || exists("tsconfig.json") {
			d.TypeScript = true
		}
		for k := range d.Deps {
			if strings.HasPrefix(k, "@wordpress/") {
				d.WordPress = true
			}
		}
	}
	if exists("go.mod") {
		d.Go = true
	}
	return d
}

// Config builds sensible defaults for what Detect found.
func (d *Detection) Config() *Config {
	c := Default()
	c.Root = d.Root
	var langs []string
	if d.PHP {
		langs = append(langs, "php")
	}
	if d.JS {
		langs = append(langs, "js")
	}
	if d.Go {
		langs = append(langs, "go")
	}
	if len(langs) == 1 {
		c.Language = langs[0]
	}

	// Tabs are only enforced by default for WordPress / PHP projects.
	c.StrictTabs = d.WordPress || d.PHP
	c.StrictShortArrays = d.PHP

	if d.PHP {
		if c.StrictTabs {
			c.Fixers = append(c.Fixers, Step{Name: "tabs", Run: "builtin:tabs", Extensions: []string{"php", "js", "css"}})
		}
		c.Fixers = append(c.Fixers, Step{Name: "short-arrays", Run: "builtin:short-arrays", Extensions: []string{"php"}})
		standard := "WordPress-VIP-Go"
		if !d.WordPress {
			standard = "PSR12"
		}
		if d.PHPCSFile != "" {
			standard = d.PHPCSFile
		}
		c.Fixers = append(c.Fixers, Step{Name: "phpcbf", Run: "phpcbf --standard=" + standard + " {files}", Extensions: []string{"php"}, OkExitCodes: []int{0, 1}})
		c.Checks = append(c.Checks,
			Step{Name: "standards", Run: "builtin:standards", Kind: "security", Extensions: []string{"php", "js", "jsx", "ts", "tsx", "css", "scss"}},
			Step{Name: "php-lint", Run: "php -l {file}", Kind: "lint", Extensions: []string{"php"}},
			Step{Name: "phpcs", Run: "phpcs --standard=" + standard + " --report=emacs -q {files}", Kind: "lint", Extensions: []string{"php"}},
		)
		if d.PHPDeps["phpunit/phpunit"] || fileExists(filepath.Join(d.Root, "phpunit.xml")) || fileExists(filepath.Join(d.Root, "phpunit.xml.dist")) {
			c.Checks = append(c.Checks, Step{Name: "phpunit", Run: "phpunit --colors=never", Kind: "test", Extensions: []string{"php"}})
		}
	}
	if d.JS {
		exts := []string{"js", "jsx", "mjs", "cjs"}
		if d.TypeScript {
			exts = append(exts, "ts", "tsx")
		}
		if d.Deps["eslint"] || fileHasPrefix(d.Root, ".eslintrc", "eslint.config") {
			c.Fixers = append(c.Fixers, Step{Name: "eslint-fix", Run: "eslint --fix --no-warn-ignored {files}", Extensions: exts, OkExitCodes: []int{0, 1}})
			c.Checks = append(c.Checks, Step{Name: "eslint", Run: "eslint -f unix --no-warn-ignored {files}", Kind: "lint", Extensions: exts})
		}
		if d.Deps["prettier"] || fileHasPrefix(d.Root, ".prettierrc", "prettier.config") {
			c.Fixers = append(c.Fixers, Step{Name: "prettier", Run: "prettier --write {files}", Extensions: append(append([]string{}, exts...), "json", "css", "scss")})
		}
		if d.Deps["stylelint"] || fileHasPrefix(d.Root, ".stylelintrc", "stylelint.config") {
			c.Fixers = append(c.Fixers, Step{Name: "stylelint-fix", Run: "stylelint --fix {files}", Extensions: []string{"css", "scss"}, OkExitCodes: []int{0, 2}})
		}
		if d.TypeScript {
			c.Checks = append(c.Checks, Step{Name: "tsc", Run: "tsc --noEmit --pretty false", Kind: "build", Extensions: []string{"ts", "tsx"}})
		}
		if d.Deps["jest"] || strings.Contains(d.Scripts["test"], "jest") {
			c.Checks = append(c.Checks, Step{Name: "jest", Run: "jest --findRelatedTests {files} --passWithNoTests --colors=false", Kind: "test", Extensions: exts})
		} else if d.Scripts["test"] != "" {
			c.Checks = append(c.Checks, Step{Name: "npm-test", Run: "npm test --silent", Kind: "test", Extensions: exts})
		}
		if d.Scripts["build"] != "" {
			c.Checks = append(c.Checks, Step{Name: "build", Run: "npm run build --silent", Kind: "build", Extensions: exts})
		}
		if !d.PHP {
			c.Checks = append([]Step{{Name: "standards", Run: "builtin:standards", Kind: "security", Extensions: []string{"js", "jsx", "ts", "tsx", "mjs", "cjs"}}}, c.Checks...)
		}
		if d.Deps["@playwright/test"] && d.Scripts["test:e2e"] != "" {
			c.Checks = append(c.Checks, Step{Name: "playwright", Run: "npm run test:e2e --silent", Kind: "test", Extensions: exts})
		}
	}
	if d.Go {
		c.Fixers = append(c.Fixers, Step{Name: "gofmt", Run: "gofmt -w {files}", Extensions: []string{"go"}})
		c.Checks = append(c.Checks,
			Step{Name: "go-vet", Run: "go vet ./...", Kind: "lint", Extensions: []string{"go"}},
			Step{Name: "go-test", Run: "go test ./...", Kind: "test", Extensions: []string{"go"}},
		)
	}
	if d.PHP {
		c.Sandbox.Link = append(c.Sandbox.Link, "vendor")
	}
	if d.JS {
		c.Sandbox.Link = append(c.Sandbox.Link, "node_modules")
	}
	return c
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func fileHasPrefix(root string, prefixes ...string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		for _, p := range prefixes {
			if strings.HasPrefix(e.Name(), p) {
				return true
			}
		}
	}
	return false
}
