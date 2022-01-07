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

package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gravitational/trace"
	"github.com/teleport/assets/backport/github"
	"gopkg.in/yaml.v2"
)

var (
	to    = flag.String("to", "", "List of comma-separated branch names to backport to.\n Ex: branch/v6,branch/v7\n")
	pr    = flag.Int("pr", 0, "Pull request with changes to backport.")
	repo  = flag.String("repo", "teleport", "Name of the repository to open up pull requests in.")
	owner = flag.String("owner", "gravitational", "Name of the repository's owner.")
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	err := parseFlags()
	if err != nil {
		log.Fatal(err)
	}

	// Getting the Github token from ~/.config/gh/hosts.yml
	token, err := getGithubToken()
	if err != nil {
		log.Fatal(err)
	}

	// Parse branches to backport to.
	backportBranches, err := parseBranches(*to)
	if err != nil {
		log.Fatal(err)
	}

	clt, err := github.New(ctx, &github.Config{
		Token:        token,
		Repository:   *repo,
		Organization: *owner,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Getting a PR from the branch name to later fill out new pull requests
	// with the original title and body.
	commits, branchName, err := clt.GetPullRequestMetadata(ctx, *pr)
	if err != nil {
		log.Fatal(err)
	}

	for _, targetBranch := range backportBranches {
		// New branches will be in the format:
		// auto-backport/[release branch name]/[original branch name]
		newBranchName, err := clt.Backport(ctx, targetBranch, branchName, commits)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Backported commits to branch %s.\n", newBranchName)

		// Create the pull request.
		err = clt.CreatePullRequest(ctx, targetBranch, newBranchName, generateTitleAndBody(*pr, targetBranch))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Pull request created for branch %s.\n", newBranchName)
	}
	fmt.Println("Backporting complete.")
}

type Config struct {
	Host Host `yaml:"github.com"`
}

type Host struct {
	Token string `yaml:"oauth_token"`
}

// githubConfigPath is the default config path
// for the Github CLI tool.
const githubConfigPath = ".config/gh/hosts.yml"

// getGithubToken gets the Github auth token from 
// the Github CLI config.
func getGithubToken() (string, error) {
	dirname, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	ghConfigPath := filepath.Join(dirname, githubConfigPath)
	yamlFile, err := ioutil.ReadFile(ghConfigPath)
	if err != nil {
		return "", trace.Wrap(err)
	}

	var config *Config = new(Config)

	err = yaml.Unmarshal(yamlFile, config)
	if err != nil {
		return "", trace.Wrap(err)
	}
	if config.Host.Token == "" {
		return "", trace.BadParameter("missing Github token.")
	}
	return config.Host.Token, nil
}

// parseFlags parses flags and sets
func parseFlags() (err error) {
	flag.Parse()
	if *to == "" {
		return trace.BadParameter("must supply branches to backport to.")
	}
	if *pr == 0 {
		return trace.BadParameter("much supply pull request with changes to backport.")
	}
	return nil
}

// parseBranches parses a string of comma separated branch
// names into a string slice.
func parseBranches(branchesInput string) ([]string, error) {
	var backportBranches []string
	branches := strings.Split(branchesInput, ",")
	for _, branch := range branches {
		if branch == "" {
			return nil, trace.BadParameter("recieved an empty branch name.")
		}
		backportBranches = append(backportBranches, strings.TrimSpace(branch))
	}
	return backportBranches, nil
}

// generateTitleAndBody generates string that will be used 
// to fill in the title and body fields for a pull request.
func generateTitleAndBody(pullNumber int, targetBranch string) string {
	return fmt.Sprintf("Backport #%s to %s", strconv.Itoa(pullNumber), targetBranch)
}
