// Copyright 2021 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/creasty/defaults"
	"github.com/google/go-github/v39/github"
	"sigs.k8s.io/yaml"
)

var (
	releaseNoteBlockRegex = regexp.MustCompile(`(?s)(?:Release note\*\*:\s*(?:<!--[^<>]*-->\s*)?` + "```(?:release-note)?|```release-note)(.+?)```")
)

type ReleaseConfig struct {
	Tag             string `json:"tag,omitempty"`
	Name            string `json:"name,omitempty"`
	Title           string `json:"title,omitempty"`
	TargetCommitish string `json:"targetCommitish,omitempty"`
	ReleaseNote     string `json:"releaseNote,omitempty"`
	Prerelease      bool   `json:"prerelease,omitempty"`

	CommitInclude ReleaseCommitMatcherConfig `json:"commitInclude,omitempty"`
	CommitExclude ReleaseCommitMatcherConfig `json:"commitExclude,omitempty"`

	CommitCategories     []ReleaseCommitCategoryConfig `json:"commitCategories,omitempty"`
	ReleaseNoteGenerator ReleaseNoteGeneratorConfig    `json:"releaseNoteGenerator,omitempty"`
}

type ReleaseCommitCategoryConfig struct {
	Id    string `json:"id,omitempty"`
	Title string `json:"title,omitempty"`
	ReleaseCommitMatcherConfig
}

type ReleaseNoteGeneratorConfig struct {
	ShowAbbrevHash         bool                       `json:"showAbbrevHash,omitempty" default:"false"`
	ShowCommitter          bool                       `json:"showCommitter,omitempty" default:"true"`
	UseReleaseNoteBlock    bool                       `json:"useReleaseNoteBlock,omitempty" default:"false"`
	UsePullRequestMetadata bool                       `json:"usePullRequestMetadata,omitempty" default:"false"`
	CommitInclude          ReleaseCommitMatcherConfig `json:"commitInclude,omitempty"`
	CommitExclude          ReleaseCommitMatcherConfig `json:"commitExclude,omitempty"`
}

type ReleaseCommitMatcherConfig struct {
	ParentOfMergeCommit bool     `json:"parentOfMergeCommit,omitempty"`
	Prefixes            []string `json:"prefixes,omitempty"`
	Contains            []string `json:"contains,omitempty"`
}

func (c ReleaseCommitMatcherConfig) Empty() bool {
	return len(c.Prefixes)+len(c.Contains) == 0
}

func (c ReleaseCommitMatcherConfig) Match(commit Commit, mergeCommit *Commit) bool {
	if c.ParentOfMergeCommit && mergeCommit != nil {
		if c.Match(*mergeCommit, nil) {
			return true
		}
	}
	for _, s := range c.Prefixes {
		if strings.HasPrefix(commit.Subject, s) {
			return true
		}
	}
	for _, s := range c.Contains {
		if strings.Contains(commit.Body, s) {
			return true
		}
	}
	return false
}

func (c *ReleaseConfig) Validate() error {
	if c.Tag == "" {
		return fmt.Errorf("tag must be specified")
	}
	return nil
}

func parseReleaseConfig(data []byte) (*ReleaseConfig, error) {
	js, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}

	c := &ReleaseConfig{}
	if err := json.Unmarshal(js, c); err != nil {
		return nil, err
	}

	if err := defaults.Set(c); err != nil {
		return nil, err
	}
	for i := range c.CommitCategories {
		if c.CommitCategories[i].Id == "" {
			c.CommitCategories[i].Id = fmt.Sprintf("_category_%d", i)
		}
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

type ReleaseProposal struct {
	Tag             string `json:"tag,omitempty"`
	Name            string `json:"name,omitempty"`
	Title           string `json:"title,omitempty"`
	TargetCommitish string `json:"targetCommitish,omitempty"`
	ReleaseNote     string `json:"releaseNote,omitempty"`
	Prerelease      bool   `json:"prerelease,omitempty"`

	Owner      string          `json:"owner,omitempty"`
	Repo       string          `json:"repo,omitempty"`
	PreTag     string          `json:"preTag,omitempty"`
	BaseCommit string          `json:"baseCommit,omitempty"`
	HeadCommit string          `json:"headCommit,omitempty"`
	Commits    []ReleaseCommit `json:"commits,omitempty"`
}

type ReleaseCommit struct {
	Commit
	ReleaseNote       string `json:"releaseNote,omitempty"`
	CategoryName      string `json:"categoryName,omitempty"`
	PullRequestNumber int    `json:"pullRequestNumber,omitempty"`
	PullRequestOwner  string `json:"pullRequestOwner,omitempty"`
}

func buildReleaseProposal(ctx context.Context, ghClient *githubClient, releaseFile string, gitExecPath, repoDir string, event *githubEvent) (*ReleaseProposal, error) {
	configLoader := func(commit string) (*ReleaseConfig, error) {
		data, err := readFileAtCommit(ctx, gitExecPath, repoDir, releaseFile, commit)
		if err != nil {
			return nil, err
		}
		return parseReleaseConfig(data)
	}

	baseCfg, err := configLoader(event.BaseCommit)
	if err != nil {
		return nil, err
	}

	headCfg, err := configLoader(event.HeadCommit)
	if err != nil {
		return nil, err
	}

	// List all commits from the last release until now.
	revisions := fmt.Sprintf("%s...%s", baseCfg.Tag, event.HeadCommit)
	commits, err := listCommits(ctx, gitExecPath, repoDir, revisions)
	if err != nil {
		return nil, err
	}

	releaseCommits, err := buildReleaseCommits(ctx, ghClient, commits, *headCfg, event)
	if err != nil {
		return nil, err
	}
	p := ReleaseProposal{
		Tag:             headCfg.Tag,
		Name:            headCfg.Name,
		Title:           headCfg.Title,
		TargetCommitish: headCfg.TargetCommitish,
		ReleaseNote:     headCfg.ReleaseNote,
		Prerelease:      headCfg.Prerelease,
		Owner:           event.Owner,
		Repo:            event.Repo,
		PreTag:          baseCfg.Tag,
		BaseCommit:      event.BaseCommit,
		HeadCommit:      event.HeadCommit,
		Commits:         releaseCommits,
	}

	if p.Title == "" {
		p.Title = fmt.Sprintf("Release %s", p.Tag)
	}
	if p.TargetCommitish == "" {
		p.TargetCommitish = event.HeadCommit
	}
	if p.ReleaseNote == "" {
		ln := renderReleaseNote(p, *headCfg)
		p.ReleaseNote = string(ln)
	}

	return &p, nil
}

func buildReleaseCommits(ctx context.Context, ghClient *githubClient, commits []Commit, cfg ReleaseConfig, event *githubEvent) ([]ReleaseCommit, error) {
	hashes := make(map[string]Commit, len(commits))
	for _, commit := range commits {
		hashes[commit.Hash] = commit
	}

	mergeCommits := make(map[string]*Commit, len(commits))
	for i := range commits {
		commit := commits[i]
		if !commit.IsMerge() {
			continue
		}
		cursor, finish := commit.ParentHashes[1], commit.ParentHashes[0]
		for {
			parent, ok := hashes[cursor]
			if !ok {
				break
			}
			if parent.Hash == finish {
				break
			}
			if len(parent.ParentHashes) != 1 {
				break
			}
			mergeCommits[cursor] = &commit
			cursor = parent.ParentHashes[0]
		}
	}

	gen, limit := cfg.ReleaseNoteGenerator, 1000
	prs := make(map[int]*github.PullRequest, limit)
	if gen.UsePullRequestMetadata {
		opts := &ListPullRequestOptions{
			State:     PullRequestStateClosed,
			Sort:      PullRequestSortUpdated,
			Direction: PullRequestDirectionDesc,
			Limit:     limit,
		}
		v, err := ghClient.listPullRequests(ctx, event.Owner, event.Repo, opts)
		if err != nil {
			return nil, err
		}
		for i := range v {
			number := *v[i].Number
			prs[number] = v[i]
		}
	}

	out := make([]ReleaseCommit, 0, len(commits))
	for _, commit := range commits {

		// Exclude was specified and matched.
		if !cfg.CommitExclude.Empty() && cfg.CommitExclude.Match(commit, mergeCommits[commit.Hash]) {
			continue
		}

		// Include was specified and not matched.
		if !cfg.CommitInclude.Empty() && !cfg.CommitInclude.Match(commit, mergeCommits[commit.Hash]) {
			continue
		}

		c := ReleaseCommit{
			Commit:       commit,
			ReleaseNote:  extractReleaseNote(commit.Subject, commit.Body, gen.UseReleaseNoteBlock),
			CategoryName: determineCommitCategory(commit, mergeCommits[commit.Hash], cfg.CommitCategories),
		}

		if gen.UsePullRequestMetadata {
			prNumber, ok := commit.PullRequestNumber()
			if !ok {
				continue
			}
			c.PullRequestNumber = prNumber

			var err error
			pr, ok := prs[prNumber]
			if !ok {
				pr, err = ghClient.getPullRequest(ctx, event.Owner, event.Repo, prNumber)
			}
			if err != nil {
				return nil, err
			}
			c.PullRequestOwner = pr.GetUser().GetLogin()
			c.ReleaseNote = extractReleaseNote(pr.GetTitle(), pr.GetBody(), gen.UseReleaseNoteBlock)
		}

		out = append(out, c)
	}
	return out, nil
}

func extractReleaseNote(def, body string, useReleaseNoteBlock bool) string {
	if !useReleaseNoteBlock {
		return def
	}

	subs := releaseNoteBlockRegex.FindStringSubmatch(body)
	if len(subs) != 2 {
		return def
	}
	if rn := strings.TrimSpace(subs[1]); rn != "" {
		return rn
	}
	return def
}

func determineCommitCategory(commit Commit, mergeCommit *Commit, categories []ReleaseCommitCategoryConfig) string {
	for _, c := range categories {
		if c.ReleaseCommitMatcherConfig.Empty() {
			return c.Id
		}
		if c.ReleaseCommitMatcherConfig.Match(commit, mergeCommit) {
			return c.Id
		}
	}
	return ""
}

func renderReleaseNote(p ReleaseProposal, cfg ReleaseConfig) []byte {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Release %s with changes since %s\n\n", p.Tag, p.PreTag))

	gen := cfg.ReleaseNoteGenerator
	renderCommit := func(c ReleaseCommit) {
		b.WriteString(fmt.Sprintf("* %s", c.ReleaseNote))
		if gen.UsePullRequestMetadata && c.PullRequestNumber != 0 {
			b.WriteString(fmt.Sprintf(" ([#%d](https://github.com/%s/%s/pull/%d))", c.PullRequestNumber, p.Owner, p.Repo, c.PullRequestNumber))
			if !gen.UseReleaseNoteBlock && c.PullRequestOwner != "" {
				b.WriteString(fmt.Sprintf(" - by @%s", c.PullRequestOwner))
			}
			b.WriteString("\n")
			return
		}
		if gen.ShowAbbrevHash {
			b.WriteString(fmt.Sprintf(" [%s](https://github.com/%s/%s/commit/%s)", c.AbbreviatedHash, p.Owner, p.Repo, c.Hash))
		}
		if gen.ShowCommitter {
			b.WriteString(fmt.Sprintf(" - by %s", c.Committer))
		}
		b.WriteString("\n")
	}

	hashes := make(map[string]Commit, len(p.Commits))
	for _, commit := range p.Commits {
		hashes[commit.Hash] = commit.Commit
	}

	mergeCommits := make(map[string]*Commit, len(p.Commits))
	for i := range p.Commits {
		commit := p.Commits[i]
		if !commit.IsMerge() {
			continue
		}
		cursor, finish := commit.ParentHashes[1], commit.ParentHashes[0]
		for {
			parent, ok := hashes[cursor]
			if !ok {
				break
			}
			if parent.Hash == finish {
				break
			}
			if len(parent.ParentHashes) != 1 {
				break
			}
			mergeCommits[cursor] = &commit.Commit
			cursor = parent.ParentHashes[0]
		}
	}

	filteredCommits := make([]ReleaseCommit, 0, len(p.Commits))
	for _, c := range p.Commits {
		// Exclude was specified and matched.
		if !cfg.ReleaseNoteGenerator.CommitExclude.Empty() && cfg.ReleaseNoteGenerator.CommitExclude.Match(c.Commit, mergeCommits[c.Hash]) {
			continue
		}
		// Include was specified and not matched.
		if !cfg.ReleaseNoteGenerator.CommitInclude.Empty() && !cfg.ReleaseNoteGenerator.CommitInclude.Match(c.Commit, mergeCommits[c.Hash]) {
			continue
		}
		filteredCommits = append(filteredCommits, c)
	}

	for _, ctg := range cfg.CommitCategories {
		commits := make([]ReleaseCommit, 0, 0)
		for _, c := range filteredCommits {
			if c.CategoryName == ctg.Id {
				commits = append(commits, c)
			}
		}
		if len(commits) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("### %s\n\n", ctg.Title))
		for _, c := range commits {
			renderCommit(c)
		}
		b.WriteString("\n")
	}

	for _, c := range filteredCommits {
		if c.CategoryName == "" {
			renderCommit(c)
		}
	}

	return []byte(b.String())
}
