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

var (
	dirs = flag.String("dirs", "", "Bazel workspaces to process")

	githubRepoRe = regexp.MustCompile(`^github.com/(.+?)/(.+?)/`)
)

type goRule struct {
	name, protoRuleName, importPath string
}

type parsedBuildFile struct {
	protoFileToRule   map[string]string
	protoRuleToGoRule map[string]goRule
}

func (b *parsedBuildFile) getGoRuleForProto(protoFile string) (*goRule, bool) {
	basename := filepath.Base(protoFile)
	protoRule, ok := b.protoFileToRule[basename]
	if !ok {
		return nil, false
	}
	goRule, ok := b.protoRuleToGoRule[protoRule]
	if !ok {
		return nil, false
	}
	return &goRule, true
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

	protoRuleToGoRule := make(map[string]goRule)

	goProtoRules := buildFile.Rules("go_proto_library")
	for _, r := range goProtoRules {
		protoRule := r.AttrString("proto")
		if protoRule == "" {
			return nil, fmt.Errorf("%s: go proto rule %q missing proto attribute", buildFilePath, r.Name())
		}
		if !strings.HasPrefix(protoRule, ":") {
			return nil, fmt.Errorf("%s: go proto rule %q has unsupported proto reference: %s", buildFilePath, r.Name(), protoRule)
		}
		importPath := r.AttrString("importpath")
		if importPath == "" {
			return nil, fmt.Errorf("%s: go proto rule %q missing importpath attribute", buildFilePath, r.Name())
		}
		protoRuleToGoRule[protoRule[1:]] = goRule{
			name:          r.Name(),
			protoRuleName: protoRule[1:],
			importPath:    importPath,
		}
	}

	return &parsedBuildFile{
		protoFileToRule:   protoFileToRule,
		protoRuleToGoRule: protoRuleToGoRule,
	}, nil
}

type result struct {
	created  int
	upToDate int
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

		goRule, ok := buildFile.getGoRuleForProto(protoFile)
		if !ok {
			return nil, fmt.Errorf("could not figure out go proto rule for %q", protoFile)
		}
		workspaceRelativePath := githubRepoRe.ReplaceAllLiteralString(goRule.importPath, "")
		if workspaceRelativePath == goRule.importPath {
			return nil, fmt.Errorf("could not figure out workspace relative path for import %s", goRule.importPath)
		}

		protoFileBasename := filepath.Base(protoFile)

		linkSrcDir := filepath.Join(workspaceRoot, workspaceRelativePath)
		if err := os.MkdirAll(linkSrcDir, 0700); err != nil {
			return nil, fmt.Errorf("could not make directory %q: %v", linkSrcDir, err)
		}
		linkSrcFile := strings.TrimSuffix(protoFileBasename, ".proto") + ".pb.go"
		linkSrc := filepath.Join(linkSrcDir, linkSrcFile)

		protoFileRelPath := strings.TrimPrefix(protoFile, workspaceRoot)
		genProtoAbsPath := filepath.Join(workspaceRoot, "bazel-bin", filepath.Dir(protoFileRelPath), goRule.name+"_", goRule.importPath, linkSrcFile)

		s, err := os.Lstat(linkSrc)
		if err == nil {
			if s.Mode()&os.ModeSymlink == 0 {
				return nil, fmt.Errorf("%s already exists and is not a symlink", linkSrc)
			}
			existingTarget, err := os.Readlink(linkSrc)
			if err != nil {
				return nil, fmt.Errorf("could not read symlink %q: %v", linkSrc, err)
			}
			// cautious for now but we should probably just overwrite the symlink
			if existingTarget != genProtoAbsPath {
				return nil, fmt.Errorf("symlink %s already exists and points to a different location", linkSrc)
			}
			result.upToDate++
		} else {
			if err := os.Symlink(genProtoAbsPath, linkSrc); err != nil {
				return nil, fmt.Errorf("could not create symlink from %q to %q: %v", genProtoAbsPath, linkSrc, err)
			}
			fmt.Printf("Created symlink for %s\n", protoFile)
			result.created++
		}
	}
	return result, nil
}

func main() {
	flag.Parse()

	for _, dir := range strings.Split(*dirs, ",") {
		result, err := processWorkspace(dir)
		if err != nil {
			fmt.Printf("Could not process workspace %s: %v\n", dir, err)
			os.Exit(1)
		}
		fmt.Printf("SYMLINKS CREATED: %d, UP TO DATE: %d\n", result.created, result.upToDate)
	}
}
