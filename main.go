package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

const (
	goProtoLibrary = "go_proto_library"
	tsProtoLibrary = "ts_proto_library"
)

var (
	dirs = flag.String("dirs", "", "Bazel workspaces to process")

	githubRepoRe = regexp.MustCompile(`^github.com/(.+?)/(.+?)/`)
)

type languageProtoRule struct {
	kind, name, protoRuleName, importPath string
}

func (r *languageProtoRule) getLinkAndTarget(workspaceRoot, protoFile string) (string, string, error) {
	protoFileRelPath := strings.TrimPrefix(protoFile, workspaceRoot)
	switch r.kind {
	case goProtoLibrary:
		workspaceRelativePath := githubRepoRe.ReplaceAllLiteralString(r.importPath, "")
		if workspaceRelativePath == r.importPath {
			return "", "", fmt.Errorf("could not figure out workspace relative path for import %q", r.importPath)
		}

		protoFileBasename := filepath.Base(protoFile)

		linkSrcDir := filepath.Join(workspaceRoot, workspaceRelativePath)
		if err := os.MkdirAll(linkSrcDir, 0700); err != nil {
			return "", "", fmt.Errorf("could not make directory %q: %v", linkSrcDir, err)
		}
		linkSrcFile := strings.TrimSuffix(protoFileBasename, ".proto") + ".pb.go"
		linkSrc := filepath.Join(linkSrcDir, linkSrcFile)

		genProtoAbsPath := filepath.Join(workspaceRoot, "bazel-bin", filepath.Dir(protoFileRelPath), r.name+"_", r.importPath, linkSrcFile)

		return linkSrc, genProtoAbsPath, nil
	case tsProtoLibrary:
		linkSrc := filepath.Join(workspaceRoot, filepath.Dir(protoFileRelPath), r.name + ".d.ts")
		genProtoAbsPath := filepath.Join(workspaceRoot, "bazel-bin", filepath.Dir(protoFileRelPath), r.name+".d.ts")
		return linkSrc, genProtoAbsPath, nil
	}
	return "", "", fmt.Errorf("unknown proto rule kind %q", r.kind)
}

type parsedBuildFile struct {
	protoFileToRule           map[string]string
	protoRuleToLangProtoRules map[string][]languageProtoRule
}

func (b *parsedBuildFile) getLangProtoRulesForProto(protoFile string) ([]languageProtoRule, bool) {
	basename := filepath.Base(protoFile)
	protoRule, ok := b.protoFileToRule[basename]
	if !ok {
		return nil, false
	}
	langRules, ok := b.protoRuleToLangProtoRules[protoRule]
	if !ok {
		return nil, false
	}
	return langRules, true
}

func parseBuildFile(buildFilePath string) (*parsedBuildFile, error) {
	buildFileContents, err := ioutil.ReadFile(buildFilePath)
	if err != nil {
		return nil, fmt.Errorf("could not read BUILD file %q: %v", buildFilePath, err)
	}
	buildFile, err := build.ParseBuild(filepath.Base(buildFilePath), buildFileContents)
	if err != nil {
		return nil, fmt.Errorf("could not parse BUILD file %q: %v", buildFilePath, err)
	}

	protoFileToRule := make(map[string]string)

	protoRules := buildFile.Rules("proto_library")
	for _, r := range protoRules {
		srcs := r.AttrStrings("srcs")
		if srcs == nil {
			return nil, fmt.Errorf("%s: proto rule %q does not have have srcs", buildFilePath, r.Name())
		}
		for _, src := range srcs {
			if protoFileToRule[src] != "" {
				return nil, fmt.Errorf("%s: src file %q appears in multiple proto rules", buildFilePath, src)
			}
			protoFileToRule[src] = r.Name()
		}
	}

	protoRuleToLangProtoRules := make(map[string][]languageProtoRule)

	goProtoRules := buildFile.Rules("")
	for _, r := range goProtoRules {
		if r.Kind() != goProtoLibrary && r.Kind() != tsProtoLibrary {
			continue
		}

		protoRule := r.AttrString("proto")
		if protoRule == "" {
			return nil, fmt.Errorf("%s: go proto rule %q missing proto attribute", buildFilePath, r.Name())
		}
		if !strings.HasPrefix(protoRule, ":") {
			return nil, fmt.Errorf("%s: go proto rule %q has unsupported proto reference: %s", buildFilePath, r.Name(), protoRule)
		}

		importPath := ""
		if r.Kind() == goProtoLibrary {
			importPath = r.AttrString("importpath")
			if importPath == "" {
				return nil, fmt.Errorf("%s: go proto rule %q missing importpath attribute", buildFilePath, r.Name())
			}
		}

		protoRuleName := protoRule[1:]
		langProtoRule := languageProtoRule{
			kind:          r.Kind(),
			name:          r.Name(),
			protoRuleName: protoRule[1:],
			importPath:    importPath,
		}
		protoRuleToLangProtoRules[protoRuleName] = append(protoRuleToLangProtoRules[protoRuleName], langProtoRule)
	}

	return &parsedBuildFile{
		protoFileToRule:           protoFileToRule,
		protoRuleToLangProtoRules: protoRuleToLangProtoRules,
	}, nil
}

type result struct {
	created  int
	upToDate int
}

func processProtoFile(workspaceRoot string, protoFile string, buildFile *parsedBuildFile, result *result) error {
	langRules, ok := buildFile.getLangProtoRulesForProto(protoFile)
	if !ok {
		return fmt.Errorf("could not figure out go proto rule for %q", protoFile)
	}

	for _, langRule := range langRules {
		link, linkTarget, err := langRule.getLinkAndTarget(workspaceRoot, protoFile)
		if err != nil {
			return err
		}

		s, err := os.Lstat(link)
		if err == nil {
			if s.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("%s already exists and is not a symlink", link)
			}
			existingTarget, err := os.Readlink(link)
			if err != nil {
				return fmt.Errorf("could not read symlink %q: %v", link, err)
			}
			// cautious for now but we should probably just overwrite the symlink
			if existingTarget != linkTarget {
				return fmt.Errorf("symlink %s already exists and points to a different location", link)
			}
			result.upToDate++
		} else {
			if err := os.Symlink(linkTarget, link); err != nil {
				return fmt.Errorf("could not create symlink from %q to %q: %v", linkTarget, link, err)
			}
			fmt.Printf("Created symlink for %s\n", protoFile)
			result.created++
		}
	}
	return nil
}

func processWorkspace(workspaceRoot string) (*result, error) {
	fmt.Printf("Processing directory %s\n", workspaceRoot)

	_, err := os.Stat(filepath.Join(workspaceRoot, "WORKSPACE"))
	if err != nil {
		return nil, fmt.Errorf("%q does not appear to be a Bazel workspace (no WORKSPACE file): %s", workspaceRoot, err)
	}
	var protoFiles []string
	err = filepath.Walk(workspaceRoot, func(path string, info os.FileInfo, err error) error {
		if !strings.HasSuffix(path, ".proto") {
			return nil
		}
		if err != nil {
			return err
		}
		protoFiles = append(protoFiles, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	result := &result{}

	buildFiles := make(map[string]*parsedBuildFile)

	for _, protoFile := range protoFiles {
		// For now only support build files named "BUILD".
		buildFilePath := filepath.Join(filepath.Dir(protoFile), "BUILD")
		// Ignore protos that are not in bazel packages.
		if _, err := os.Stat(buildFilePath); err != nil {
			continue
		}
		buildFile := buildFiles[buildFilePath]
		if buildFile == nil {
			buildFile, err = parseBuildFile(buildFilePath)
			if err != nil {
				return nil, fmt.Errorf("could not parse BUILD file %q: %v", buildFilePath, err)
			}
			buildFiles[buildFilePath] = buildFile
		}

		if err := processProtoFile(workspaceRoot, protoFile, buildFile, result); err != nil {
			return nil, err
		}

	}
	return result, nil
}

func main() {
	flag.Parse()

	if *dirs == "" {
		fmt.Printf("Please specify --dirs")
		os.Exit(1)
	}

	for _, dir := range strings.Split(*dirs, ",") {
		result, err := processWorkspace(dir)
		if err != nil {
			fmt.Printf("Could not process workspace %s: %v\n", dir, err)
			os.Exit(1)
		}
		fmt.Printf("SYMLINKS CREATED: %d, UP TO DATE: %d\n", result.created, result.upToDate)
	}
}
