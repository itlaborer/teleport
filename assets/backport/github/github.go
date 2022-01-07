/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/gravitational/trace"

	go_github "github.com/google/go-github/v41/github"
	"golang.org/x/oauth2"
)

type Client struct {
	Client *go_github.Client
	Config
}

type Config struct {
	Token        string
	Organization string
	Repository   string
}

// New returns a new GitHub client.
func New(ctx context.Context, c *Config) (*Client, error) {
	if err := c.Check(); err != nil {
		return nil, trace.Wrap(err)
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: c.Token},
	)
	return &Client{
		Client: go_github.NewClient(oauth2.NewClient(ctx, ts)),
		Config: *c,
	}, nil
}

// Check validates config.
func (c *Config) Check() error {
	if c.Token == "" {
		return trace.BadParameter("missing token")
	}
	if c.Organization == "" {
		return trace.BadParameter("missing organization")
	}
	if c.Repository == "" {
		return trace.BadParameter("missing repository")
	}
	return nil
}

// Backport backports changes from backportBranchName to a new branch based
// off baseBranchName.
//
// A new branch is created with the name in the format of
// auto-backport/[baseBranchName]/[backportBranchName], and
// cherry-picks commits onto the new branch.
func (c *Client) Backport(ctx context.Context, baseBranchName, backportBranchName string, commits []string) (string, error) {
	newBranchName := fmt.Sprintf("auto-backport/%s/%s", baseBranchName, backportBranchName)
	// Create a new branch off of the target branch.
	err := c.createBranchFrom(ctx, baseBranchName, newBranchName)
	if err != nil {
		return "", trace.Wrap(err)
	}
	fmt.Printf("Created a new branch: %s.\n", newBranchName)

	// Cherry pick commits.
	err = c.cherryPickCommitsOnBranch(ctx, newBranchName, commits)
	if err != nil {
		return "", trace.Wrap(err)
	}
	fmt.Printf("Finished cherry-picking %v commits. \n", len(commits))
	return newBranchName, nil
}

// CreatePullRequest creates a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, baseBranch string, headBranch string, titleAndBody string) error {
	newPR := &go_github.NewPullRequest{
		Title:               go_github.String(titleAndBody),
		Head:                go_github.String(headBranch),
		Base:                go_github.String(baseBranch),
		Body:                go_github.String(titleAndBody),
		MaintainerCanModify: go_github.Bool(true),
	}
	_, _, err := c.Client.PullRequests.Create(ctx, c.Organization, c.Repository, newPR)
	if err != nil {
		return err
	}
	return nil
}

// GetPullRequestMetadata gets the commit shas, title, and body for a pull request
// associated with the passed in branch name.
func (c *Client) GetPullRequestMetadata(ctx context.Context, number int) (commits []string, branchName string, err error) {
	pull, _, err := c.Client.PullRequests.Get(ctx, c.Organization, c.Repository, number)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	if pull.GetState() != backportPRState {
		return nil, "", trace.Errorf("pull request %v is not closed", number)
	}
	if strings.TrimPrefix(pull.GetBase().GetRef(), branchRefPrefix) != masterBranchName {
		return nil, "", trace.Errorf("pull request %v's base is not master", number)
	}

	commits, err = c.getPullRequestCommits(ctx, pull.GetNumber())
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	return commits, strings.TrimPrefix(pull.GetHead().GetRef(), branchRefPrefix), nil
}

// cherryPickCommitsOnBranch cherry picks a list of commits on a given branch.
func (c *Client) cherryPickCommitsOnBranch(ctx context.Context, branchName string, commits []string) error {
	branch, _, err := c.Client.Repositories.GetBranch(ctx, c.Organization, c.Repository, branchName, true)
	if err != nil {
		return trace.Wrap(err)
	}
	// Get the branch's HEAD.
	headCommit, _, err := c.Client.Git.GetCommit(ctx,
		c.Organization,
		c.Repository,
		branch.GetCommit().GetSHA())
	if err != nil {
		return trace.Wrap(err)
	}

	for i := 0; i < len(commits); i++ {
		cherryCommit, _, err := c.Client.Git.GetCommit(ctx, c.Organization, c.Repository, commits[i])
		if err != nil {
			return trace.Wrap(err)
		}
		tree, sha, err := c.cherryPickCommit(ctx, branchName, cherryCommit, headCommit)
		if err != nil {
			defer func() {
				refName := fmt.Sprintf("%s%s", branchRefPrefix, branchName)
				c.Client.Git.DeleteRef(ctx, c.Organization, c.Repository, refName)
			}()
			return trace.Wrap(err)
		}
		headCommit.SHA = &sha
		headCommit.Tree = tree
	}
	return nil
}

// cherryPickCommit cherry picks a single commit on a branch.
func (c *Client) cherryPickCommit(ctx context.Context, branchName string, cherryCommit, headBranchCommit *go_github.Commit) (*go_github.Tree, string, error) {
	if len(cherryCommit.Parents) != 1 {
		return nil, "", trace.BadParameter("merge commits are not supported")
	}
	cherryParent := cherryCommit.Parents[0]
	// Temporarily set the parent of the branch HEAD to the parent of the commit
	// to cherry-pick so they are siblings.
	err := c.createSiblingCommit(ctx, branchName, headBranchCommit, cherryParent)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}

	// When git performs the merge, it detects that the parent of the branch commit that is
	// being merged onto matches the parent of the cherry pick commit, and merges a tree of size 1.
	// The merge commit will contain the delta between the file tree in target branch and the
	// commit to cherry-pick.
	merge, err := c.merge(ctx, branchName, cherryCommit.GetSHA())
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	mergeTree := merge.GetTree()

	updatedCommit, _, err := c.Client.Git.GetCommit(ctx,
		c.Organization,
		c.Repository,
		headBranchCommit.GetSHA())
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	// Create the actual cherry-pick commit on the target branch containing the merge commit tree.
	commit, _, err := c.Client.Git.CreateCommit(ctx, c.Organization, c.Repository, &go_github.Commit{
		Message: cherryCommit.Message,
		Tree:    mergeTree,
		Parents: []*go_github.Commit{
			updatedCommit,
		},
	})
	if err != nil {
		return nil, "", trace.Wrap(err)
	}

	// Overwrite the merge commit and its parent on the branch by the newly created commit.
	// The result will be equivalent to what would have happened with a fast-forward merge.
	sha := commit.GetSHA()
	refName := fmt.Sprintf("%s%s", branchRefPrefix, branchName)
	_, _, err = c.Client.Git.UpdateRef(ctx, c.Organization, c.Repository, &go_github.Reference{
		Ref: go_github.String(refName),
		Object: &go_github.GitObject{
			SHA: go_github.String(sha),
		},
	}, true)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	return mergeTree, sha, nil
}

// createSiblingCommit creates a commit with the passed in commit's tree and parent
// and updates the passed in branch to point at that commit.
func (c *Client) createSiblingCommit(ctx context.Context, branchName string, branchHeadCommit *go_github.Commit, cherryParent *go_github.Commit) error {
	tree := branchHeadCommit.GetTree()

	// This will be the temp commit, commit is lost.
	commit, _, err := c.Client.Git.CreateCommit(ctx, c.Organization, c.Repository, &go_github.Commit{
		Message: go_github.String("field-not-required"),
		Tree:    tree,
		Parents: []*go_github.Commit{
			cherryParent,
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}
	sha := commit.GetSHA()

	refName := fmt.Sprintf("%s%s", branchRefPrefix, branchName)
	_, _, err = c.Client.Git.UpdateRef(ctx, c.Organization, c.Repository, &go_github.Reference{
		Ref: go_github.String(refName),
		Object: &go_github.GitObject{
			SHA: go_github.String(sha),
		},
	}, true)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// createBranchFrom creates a branch from the passed in branch's HEAD.
func (c *Client) createBranchFrom(ctx context.Context, branchFromName string, newBranchName string) error {
	baseBranch, _, err := c.Client.Repositories.GetBranch(ctx, c.Organization, c.Repository, branchFromName, true)
	if err != nil {
		return trace.Wrap(err)
	}
	newRefBranchName := fmt.Sprintf("%s%s", branchRefPrefix, newBranchName)
	baseBranchSHA := baseBranch.GetCommit().GetSHA()

	ref := &go_github.Reference{
		Ref: go_github.String(newRefBranchName),
		Object: &go_github.GitObject{
			SHA: go_github.String(baseBranchSHA), /* SHA to branch from */
		},
	}
	_, _, err = c.Client.Git.CreateRef(ctx, c.Organization, c.Repository, ref)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// merge merges a branch.
func (c *Client) merge(ctx context.Context, base string, headCommitSHA string) (*go_github.Commit, error) {
	merge, _, err := c.Client.Repositories.Merge(ctx, c.Organization, c.Repository, &go_github.RepositoryMergeRequest{
		Base: go_github.String(base),
		Head: go_github.String(headCommitSHA),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	mergeCommit, _, err := c.Client.Git.GetCommit(ctx,
		c.Organization,
		c.Repository,
		merge.GetSHA())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return mergeCommit, nil
}

func (c *Client) getPullRequestCommits(ctx context.Context, number int) ([]string, error) {
	var commitSHAs []string
	opts := go_github.ListOptions{
		Page:    0,
		PerPage: perPage,
	}
	for {
		currCommits, resp, err := c.Client.PullRequests.ListCommits(ctx,
			c.Organization,
			c.Repository,
			number, &go_github.ListOptions{})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		for _, commit := range currCommits {
			commitSHAs = append(commitSHAs, commit.GetSHA())
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}
	return commitSHAs, nil
}

const (
	// backportPRState is the state a pull request should be
	// to backport changes.
	backportPRState = "closed"

	// masterBranchName is the master branch name.
	masterBranchName = "master"

	// perPage is the number of items per page to request.
	perPage = 100

	// branchRefPrefix is the prefix for a reference that is
	// pointing to a branch.
	branchRefPrefix = "refs/heads/"
)
