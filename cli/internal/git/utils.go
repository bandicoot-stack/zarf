package git

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	netHttp "net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/defenseunicorns/zarf/cli/config"
	"github.com/defenseunicorns/zarf/cli/internal/k8s"
	"github.com/defenseunicorns/zarf/cli/internal/message"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

type Credential struct {
	Path string
	Auth http.BasicAuth
}

func MutateGitUrlsInText(host string, text string) string {
	extractPathRegex := regexp.MustCompilePOSIX(`https?://[^/]+/(.*\.git)`)
	output := extractPathRegex.ReplaceAllStringFunc(text, func(match string) string {
		if strings.Contains(match, config.ZarfGitPushUser) {
			message.Warnf("%s seems to have been previously patched.", match)
			return match
		}
		return transformURL(host, match)
	})
	return output
}

func transformURLtoRepoName(url string) string {
	replaceRegex := regexp.MustCompile(`(https?://|[^\w\-.])+`)
	return "mirror" + replaceRegex.ReplaceAllString(url, "__")
}

func transformURL(baseUrl string, url string) string {
	replaced := transformURLtoRepoName(url)
	output := baseUrl + "/" + config.ZarfGitPushUser + "/" + replaced
	message.Debugf("Rewrite git URL: %s -> %s", url, output)
	return output
}

func credentialFilePath() string {
	homePath, _ := os.UserHomeDir()
	return homePath + "/.git-credentials"
}

func credentialParser() []Credential {
	credentialsPath := credentialFilePath()
	var credentials []Credential

	credentialsFile, _ := os.Open(credentialsPath)
	defer func(credentialsFile *os.File) {
		err := credentialsFile.Close()
		if err != nil {
			message.Debugf("Unable to load an existing git credentials file: %w", err)
		}
	}(credentialsFile)

	scanner := bufio.NewScanner(credentialsFile)
	for scanner.Scan() {
		gitUrl, err := url.Parse(scanner.Text())
		password, _ := gitUrl.User.Password()
		if err != nil {
			continue
		}
		credential := Credential{
			Path: gitUrl.Host,
			Auth: http.BasicAuth{
				Username: gitUrl.User.Username(),
				Password: password,
			},
		}
		credentials = append(credentials, credential)
	}

	return credentials
}

func FindAuthForHost(baseUrl string) Credential {
	// Read the ~/.git-credentials file
	gitCreds := credentialParser()

	// Will be nil unless a match is found
	var matchedCred Credential

	// Look for a match for the given host path in the creds file
	for _, gitCred := range gitCreds {
		hasPath := strings.Contains(baseUrl, gitCred.Path)
		if hasPath {
			matchedCred = gitCred
			break
		}
	}

	return matchedCred
}

// removeLocalBranchRefs removes all refs that are local branches
// It returns a slice of references deleted
func removeLocalBranchRefs(gitDirectory string) ([]*plumbing.Reference, error) {
	return removeReferences(
		gitDirectory,
		func(ref *plumbing.Reference) bool {
			return ref.Name().IsBranch()
		},
	)
}

// removeOnlineRemoteRefs removes all refs pointing to the online-upstream
// It returns a slice of references deleted
func removeOnlineRemoteRefs(gitDirectory string) ([]*plumbing.Reference, error) {
	return removeReferences(
		gitDirectory,
		func(ref *plumbing.Reference) bool {
			return strings.HasPrefix(ref.Name().String(), onlineRemoteRefPrefix)
		},
	)
}

// removeHeadCopies removes any refs that aren't HEAD but have the same hash
// It returns a slice of references deleted
func removeHeadCopies(gitDirectory string) ([]*plumbing.Reference, error) {
	message.Debugf("Remove head copies for %s", gitDirectory)
	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return nil, fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to identify references when getting the repo's head: %w", err)
	}

	headHash := head.Hash().String()
	return removeReferences(
		gitDirectory,
		func(ref *plumbing.Reference) bool {
			// Don't ever remove tags
			return !ref.Name().IsTag() && ref.Hash().String() == headHash
		},
	)
}

// removeReferences removes references based on a provided callback
// removeReferences does not allow you to delete HEAD
// It returns a slice of references deleted
func removeReferences(
	gitDirectory string,
	shouldRemove func(*plumbing.Reference) bool,
) ([]*plumbing.Reference, error) {
	message.Debugf("Remove git references %s", gitDirectory)
	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return nil, fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	references, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to identify references when getting the repo's references: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to identify head: %w", err)
	}

	var removedRefs []*plumbing.Reference
	err = references.ForEach(func(ref *plumbing.Reference) error {
		refIsNotHeadOrHeadTarget := ref.Name() != plumbing.HEAD && ref.Name() != head.Name()
		// Run shouldRemove inline here to take advantage of short circuit
		// evaluation as to not waste a cycle on HEAD
		if refIsNotHeadOrHeadTarget && shouldRemove(ref) {
			err = repo.Storer.RemoveReference(ref.Name())
			if err != nil {
				return err
			}
			removedRefs = append(removedRefs, ref)
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to remove references: %w", err)
	}

	return removedRefs, nil
}

// addRefs adds a provided arbitrary list of references to a repo
// It is intended to be used with references returned by a Remove function
func addRefs(gitDirectory string, refs []*plumbing.Reference) error {
	message.Debugf("Add git refs %s", gitDirectory)
	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	for _, ref := range refs {
		err = repo.Storer.SetReference(ref)
		if err != nil {
			return fmt.Errorf("failed to add references: %w", err)
		}
	}

	return nil
}

// deleteBranchIfExists ensures the provided branch name does not exist
func deleteBranchIfExists(gitDirectory string, branchName plumbing.ReferenceName) error {
	message.Debugf("Delete branch %s for %s if it exists", branchName.String(), gitDirectory)

	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	// Deletes the branch by name
	err = repo.DeleteBranch(branchName.Short())
	if err != nil && err != git.ErrBranchNotFound {
		return fmt.Errorf("failed to delete branch: %w", err)
	}

	// Delete reference too
	err = repo.Storer.RemoveReference(branchName)
	if err != nil && err != git.ErrInvalidReference {
		return fmt.Errorf("failed to delete branch reference: %w", err)
	}

	return nil
}

func CreateZarfOrg() error {
	// Establish a git tunnel to send the repos
	tunnel := k8s.NewZarfTunnel()
	tunnel.Connect(k8s.ZarfGit, false)
	defer tunnel.Close()

	body := map[string]string{
		"username":   config.ZarfGitOrg,
		"visibility": "limited",
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return err
	}

	request, err := netHttp.NewRequest("POST", fmt.Sprintf("http://%s:%d/api/v1/orgs", config.IPV4Localhost, k8s.PortGit), bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	request.SetBasicAuth(config.ZarfGitPushUser, config.GetSecret(config.StateGitPush))
	request.Header.Add("accept", "application/json")
	request.Header.Add("Content-Type", "application/json")

	client := &netHttp.Client{Timeout: time.Second * 10}
	createOrgResponse, err := client.Do(request)
	if err != nil || createOrgResponse.StatusCode < 200 || createOrgResponse.StatusCode >= 300 {
		createOrgResponseBody, _ := io.ReadAll(createOrgResponse.Body)
		message.Debugf("Editing the read-only user permissions failed with a status-code of %v and a response body of: %v\n", createOrgResponse.Status, createOrgResponseBody)

		if err == nil {
			err = errors.New("unable to create zarf org")
		}
		return err
	}
	return err
}

func CreateReadOnlyUser() error {
	// Establish a git tunnel to send the repo
	tunnel := k8s.NewZarfTunnel()
	tunnel.Connect(k8s.ZarfGit, false)
	defer tunnel.Close()

	client := &netHttp.Client{Timeout: time.Second * 10}

	// Create the user
	createUserBody := map[string]interface{}{
		"username":             config.ZarfGitReadUser,
		"password":             config.GetSecret(config.StateGitPull),
		"email":                "zarf-reader@localhost.local",
		"must_change_password": false,
	}
	createUserData, err := json.Marshal(createUserBody)
	if err != nil {
		return err
	}
	createUserRequest, err := netHttp.NewRequest("POST", fmt.Sprintf("http://%s:%d/api/v1/admin/users", config.IPV4Localhost, k8s.PortGit), bytes.NewBuffer(createUserData))
	if err != nil {
		return err
	}
	createUserRequest.SetBasicAuth(config.ZarfGitPushUser, config.GetSecret(config.StateGitPush))
	createUserRequest.Header.Add("accept", "application/json")
	createUserRequest.Header.Add("Content-Type", "application/json")
	createUserResponse, err := client.Do(createUserRequest)
	if err != nil || createUserResponse.StatusCode < 200 || createUserResponse.StatusCode >= 300 {
		createUserResponseBody, _ := io.ReadAll(createUserResponse.Body)
		message.Debugf("Editing the read-only user permissions failed with a status-code of %v and a response body of: %v\n", createUserResponse.Status, createUserResponseBody)
		if err == nil {
			err = errors.New("unable to create zarf read-only user")
		}
		return err
	}

	// Make sure the user can't create their own repos or orgs
	updateUserBody := map[string]interface{}{
		"email":                     "zarf-reader@localhost.local",
		"max_repo_creation":         0,
		"allow_create_organization": false,
	}
	updateUserData, _ := json.Marshal(updateUserBody)
	updateUserRequest, _ := netHttp.NewRequest("PATCH", fmt.Sprintf("http://%s:%d/api/v1/admin/users/%s", config.IPV4Localhost, k8s.PortGit, config.ZarfGitReadUser), bytes.NewBuffer(updateUserData))
	updateUserRequest.SetBasicAuth(config.ZarfGitPushUser, config.GetSecret(config.StateGitPush))
	updateUserRequest.Header.Add("accept", "application/json")
	updateUserRequest.Header.Add("Content-Type", "application/json")
	updateUserResponse, err := client.Do(updateUserRequest)
	if err != nil || updateUserResponse.StatusCode < 200 || updateUserResponse.StatusCode >= 300 {
		updateUserResponseBody, _ := io.ReadAll(updateUserResponse.Body)
		message.Debugf("Editing the read-only user permissions failed with a status-code of %v and a response body of: %v\n", updateUserResponse.Status, updateUserResponseBody)

		if err == nil {
			err = errors.New("unable to update zarf read-only user")
		}
		return err
	}
	return err
}

func addReadOnlyUser(repo string) error {
	client := &netHttp.Client{Timeout: time.Second * 10}

	// Add the readonly user to the repo
	addColabBody := map[string]string{
		"permission": "read",
	}
	addColabData, err := json.Marshal(addColabBody)
	if err != nil {
		return err
	}
	addColabRequest, err := netHttp.NewRequest("PUT", fmt.Sprintf("http://%s:%d/api/v1/repos/%s/%s/collaborators/%s", config.IPV4Localhost, k8s.PortGit, config.ZarfGitPushUser, repo, config.ZarfGitReadUser), bytes.NewBuffer(addColabData))
	if err != nil {
		return err
	}
	addColabRequest.SetBasicAuth(config.ZarfGitPushUser, config.GetSecret(config.StateGitPush))
	addColabRequest.Header.Add("accept", "application/json")
	addColabRequest.Header.Add("Content-Type", "application/json")
	response, err := client.Do(addColabRequest)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(response.Body)
		message.Debugf("Adding the read-only user to the %v repo failed with a status-code of %v and a response body of: %v\n", repo, response.Status, responseBody)

		if err == nil {
			err = errors.New("unable to add read-only user to repo")
		}
		return err
	}

	return err
}
