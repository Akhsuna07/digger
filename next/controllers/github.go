package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/diggerhq/digger/backend/ci_backends"
	orchestrator_scheduler "github.com/diggerhq/digger/libs/scheduler"
	"github.com/diggerhq/digger/next/model"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"

	"github.com/diggerhq/digger/backend/middleware"
	"github.com/diggerhq/digger/backend/utils"
	dg_github "github.com/diggerhq/digger/libs/ci/github"
	dg_configuration "github.com/diggerhq/digger/libs/digger_config"
	"github.com/diggerhq/digger/next/models"
	"github.com/dominikbraun/graph"
	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v61/github"
	"github.com/samber/lo"
	"golang.org/x/oauth2"
)

type DiggerController struct {
	CiBackendProvider    ci_backends.CiBackendProvider
	GithubClientProvider utils.GithubClientProvider
}

func (d DiggerController) GithubAppWebHook(c *gin.Context) {
	c.Header("Content-Type", "application/json")
	gh := d.GithubClientProvider
	log.Printf("GithubAppWebHook")

	payload, err := github.ValidatePayload(c.Request, []byte(os.Getenv("GITHUB_WEBHOOK_SECRET")))
	if err != nil {
		log.Printf("Error validating github app webhook's payload: %v", err)
		c.String(http.StatusBadRequest, "Error validating github app webhook's payload")
		return
	}

	webhookType := github.WebHookType(c.Request)
	event, err := github.ParseWebHook(webhookType, payload)
	if err != nil {
		log.Printf("Failed to parse Github Event. :%v\n", err)
		c.String(http.StatusInternalServerError, "Failed to parse Github Event")
		return
	}

	log.Printf("github event type: %v\n", reflect.TypeOf(event))

	switch event := event.(type) {
	case *github.InstallationEvent:
		log.Printf("InstallationEvent, action: %v\n", *event.Action)
		if *event.Action == "created" {
			err := handleInstallationCreatedEvent(event)
			if err != nil {
				c.String(http.StatusInternalServerError, "Failed to handle webhook event.")
				return
			}
		}

		if *event.Action == "deleted" {
			err := handleInstallationDeletedEvent(event)
			if err != nil {
				c.String(http.StatusInternalServerError, "Failed to handle webhook event.")
				return
			}
		}
	case *github.InstallationRepositoriesEvent:
		log.Printf("InstallationRepositoriesEvent, action: %v\n", *event.Action)
		if *event.Action == "added" {
			err := handleInstallationRepositoriesAddedEvent(gh, event)
			if err != nil {
				c.String(http.StatusInternalServerError, "Failed to handle installation repo added event.")
			}
		}
		if *event.Action == "removed" {
			err := handleInstallationRepositoriesDeletedEvent(event)
			if err != nil {
				c.String(http.StatusInternalServerError, "Failed to handle installation repo deleted event.")
			}
		}
	case *github.IssueCommentEvent:
		log.Printf("IssueCommentEvent, action: %v\n", *event.Action)
	case *github.PullRequestEvent:
		log.Printf("Got pull request event for %d", *event.PullRequest.ID)
	case *github.PushEvent:
		log.Printf("Got push event for %d", event.Repo.URL)
	default:
		log.Printf("Unhandled event, event type %v", reflect.TypeOf(event))
	}

	c.JSON(200, "ok")
}

func GithubAppSetup(c *gin.Context) {

	type githubWebhook struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}

	type githubAppRequest struct {
		Description           string            `json:"description"`
		Events                []string          `json:"default_events"`
		Name                  string            `json:"name"`
		Permissions           map[string]string `json:"default_permissions"`
		Public                bool              `json:"public"`
		RedirectURL           string            `json:"redirect_url"`
		CallbackUrls          []string          `json:"callback_urls"`
		RequestOauthOnInstall bool              `json:"request_oauth_on_install"`
		SetupOnUpdate         bool              `json:"setup_on_update"'`
		URL                   string            `json:"url"`
		Webhook               *githubWebhook    `json:"hook_attributes"`
	}

	host := os.Getenv("HOSTNAME")
	manifest := &githubAppRequest{
		Name:        fmt.Sprintf("Digger app %v", rand.Int31()),
		Description: fmt.Sprintf("Digger hosted at %s", host),
		URL:         host,
		RedirectURL: fmt.Sprintf("%s/github/exchange-code", host),
		Public:      false,
		Webhook: &githubWebhook{
			Active: true,
			URL:    fmt.Sprintf("%s/github-app-webhook", host),
		},
		CallbackUrls:          []string{fmt.Sprintf("%s/github/callback", host)},
		SetupOnUpdate:         true,
		RequestOauthOnInstall: true,
		Events: []string{
			"check_run",
			"create",
			"delete",
			"issue_comment",
			"issues",
			"status",
			"pull_request_review_thread",
			"pull_request_review_comment",
			"pull_request_review",
			"pull_request",
			"push",
		},
		Permissions: map[string]string{
			"actions":          "write",
			"contents":         "write",
			"issues":           "write",
			"pull_requests":    "write",
			"repository_hooks": "write",
			"statuses":         "write",
			"administration":   "read",
			"checks":           "write",
			"members":          "read",
			"workflows":        "write",
		},
	}

	githubHostname := getGithubHostname()
	url := &url.URL{
		Scheme: "https",
		Host:   githubHostname,
		Path:   "/settings/apps/new",
	}

	// https://developer.github.com/apps/building-github-apps/creating-github-apps-using-url-parameters/#about-github-app-url-parameters
	githubOrg := os.Getenv("GITHUB_ORG")
	if githubOrg != "" {
		url.Path = fmt.Sprintf("organizations/%s%s", githubOrg, url.Path)
	}

	jsonManifest, err := json.MarshalIndent(manifest, "", " ")
	if err != nil {
		c.Error(fmt.Errorf("failed to serialize manifest %s", err))
		return
	}

	c.HTML(http.StatusOK, "github_setup.tmpl", gin.H{"Target": url.String(), "Manifest": string(jsonManifest)})
}

func getGithubHostname() string {
	githubHostname := os.Getenv("DIGGER_GITHUB_HOSTNAME")
	if githubHostname == "" {
		githubHostname = "github.com"
	}
	return githubHostname
}

// GithubSetupExchangeCode handles the user coming back from creating their app
// A code query parameter is exchanged for this app's ID, key, and webhook_secret
// Implements https://developer.github.com/apps/building-github-apps/creating-github-apps-from-a-manifest/#implementing-the-github-app-manifest-flow
func (d DiggerController) GithubSetupExchangeCode(c *gin.Context) {
	code := c.Query("code")
	if code == "" {
		c.Error(fmt.Errorf("Ignoring callback, missing code query parameter"))
	}

	// TODO: to make tls verification configurable for debug purposes
	//var transport *http.Transport = nil
	//_, exists := os.LookupEnv("DIGGER_GITHUB_SKIP_TLS")
	//if exists {
	//	transport = &http.Transport{
	//		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	//	}
	//}

	client, err := d.GithubClientProvider.NewClient(nil)
	if err != nil {
		c.Error(fmt.Errorf("could not create github client: %v", err))
	}
	cfg, _, err := client.Apps.CompleteAppManifest(context.Background(), code)
	if err != nil {
		c.Error(fmt.Errorf("Failed to exchange code for github app: %s", err))
		return
	}
	log.Printf("Found credentials for GitHub app %v with id %d", *cfg.Name, cfg.GetID())

	_, err = models.DB.CreateGithubApp(cfg.GetName(), cfg.GetID(), cfg.GetHTMLURL())
	if err != nil {
		c.Error(fmt.Errorf("Failed to create github app record on callback"))
	}

	PEM := cfg.GetPEM()
	PemBase64 := base64.StdEncoding.EncodeToString([]byte(PEM))
	c.HTML(http.StatusOK, "github_setup.tmpl", gin.H{
		"Target":        "",
		"Manifest":      "",
		"ID":            cfg.GetID(),
		"ClientID":      cfg.GetClientID(),
		"ClientSecret":  cfg.GetClientSecret(),
		"Key":           PEM,
		"KeyBase64":     PemBase64,
		"WebhookSecret": cfg.GetWebhookSecret(),
		"URL":           cfg.GetHTMLURL(),
	})

}

func createOrGetDiggerRepoForGithubRepo(ghRepoFullName string, ghRepoOrganisation string, ghRepoName string, ghRepoUrl string, installationId int64) (*model.Repo, *model.Organization, error) {
	link, err := models.DB.GetGithubInstallationLinkForInstallationId(installationId)
	if err != nil {
		log.Printf("Error fetching installation link: %v", err)
		return nil, nil, err
	}
	orgId := link.OrganizationID
	org, err := models.DB.GetOrganisationById(orgId)
	if err != nil {
		log.Printf("Error fetching organisation by id: %v, error: %v\n", orgId, err)
		return nil, nil, err
	}

	diggerRepoName := strings.ReplaceAll(ghRepoFullName, "/", "-")

	repo, err := models.DB.GetRepo(orgId, diggerRepoName)

	if err != nil {
		log.Printf("Error fetching repo: %v", err)
		return nil, nil, err
	}

	if repo != nil {
		log.Printf("Digger repo already exists: %v", repo)
		return repo, org, nil
	}

	repo, err = models.DB.CreateRepo(diggerRepoName, ghRepoFullName, ghRepoOrganisation, ghRepoName, ghRepoUrl, org, `
generate_projects:
 include: "."
`)
	if err != nil {
		log.Printf("Error creating digger repo: %v", err)
		return nil, nil, err
	}
	log.Printf("Created digger repo: %v", repo)
	return repo, org, nil
}

func handleInstallationRepositoriesAddedEvent(ghClientProvider utils.GithubClientProvider, payload *github.InstallationRepositoriesEvent) error {
	installationId := *payload.Installation.ID
	login := *payload.Installation.Account.Login
	accountId := *payload.Installation.Account.ID
	appId := *payload.Installation.AppID

	for _, repo := range payload.RepositoriesAdded {
		repoFullName := *repo.FullName
		repoOwner := strings.Split(*repo.FullName, "/")[0]
		repoName := *repo.Name
		repoUrl := fmt.Sprintf("https://github.com/%v", repoFullName)
		_, err := models.DB.GithubRepoAdded(installationId, appId, login, accountId, repoFullName)
		if err != nil {
			log.Printf("GithubRepoAdded failed, error: %v\n", err)
			return err
		}

		_, _, err = createOrGetDiggerRepoForGithubRepo(repoFullName, repoOwner, repoName, repoUrl, installationId)
		if err != nil {
			log.Printf("createOrGetDiggerRepoForGithubRepo failed, error: %v\n", err)
			return err
		}
	}
	return nil
}

func handleInstallationRepositoriesDeletedEvent(payload *github.InstallationRepositoriesEvent) error {
	installationId := *payload.Installation.ID
	appId := *payload.Installation.AppID
	for _, repo := range payload.RepositoriesRemoved {
		repoFullName := *repo.FullName
		_, err := models.DB.GithubRepoRemoved(installationId, appId, repoFullName)
		if err != nil {
			return err
		}

		// todo: change the status of DiggerRepo to InActive
	}
	return nil
}

func handleInstallationCreatedEvent(installation *github.InstallationEvent) error {
	installationId := *installation.Installation.ID
	login := *installation.Installation.Account.Login
	accountId := *installation.Installation.Account.ID
	appId := *installation.Installation.AppID

	for _, repo := range installation.Repositories {
		repoFullName := *repo.FullName
		repoOwner := strings.Split(*repo.FullName, "/")[0]
		repoName := *repo.Name
		repoUrl := fmt.Sprintf("https://github.com/%v", repoFullName)

		log.Printf("Adding a new installation %d for repo: %s", installationId, repoFullName)
		_, err := models.DB.GithubRepoAdded(installationId, appId, login, accountId, repoFullName)
		if err != nil {
			return err
		}
		_, _, err = createOrGetDiggerRepoForGithubRepo(repoFullName, repoOwner, repoName, repoUrl, installationId)
		if err != nil {
			return err
		}
	}
	return nil
}

func handleInstallationDeletedEvent(installation *github.InstallationEvent) error {
	installationId := *installation.Installation.ID
	appId := *installation.Installation.AppID

	link, err := models.DB.GetGithubInstallationLinkForInstallationId(installationId)
	if err != nil {
		return err
	}
	_, err = models.DB.MakeGithubAppInstallationLinkInactive(link)
	if err != nil {
		return err
	}

	for _, repo := range installation.Repositories {
		repoFullName := *repo.FullName
		log.Printf("Removing an installation %d for repo: %s", installationId, repoFullName)
		_, err := models.DB.GithubRepoRemoved(installationId, appId, repoFullName)
		if err != nil {
			return err
		}
	}
	return nil
}

func handlePushEvent(gh utils.GithubClientProvider, payload *github.PushEvent) error {
	installationId := *payload.Installation.ID
	repoName := *payload.Repo.Name
	repoFullName := *payload.Repo.FullName
	repoOwner := *payload.Repo.Owner.Login
	cloneURL := *payload.Repo.CloneURL
	ref := *payload.Ref
	defaultBranch := *payload.Repo.DefaultBranch

	link, err := models.DB.GetGithubAppInstallationLink(installationId)
	if err != nil {
		log.Printf("Error getting GetGithubAppInstallationLink: %v", err)
		return fmt.Errorf("error getting github app link")
	}

	orgId := link.OrganizationID
	diggerRepoName := strings.ReplaceAll(repoFullName, "/", "-")
	repo, err := models.DB.GetRepo(orgId, diggerRepoName)
	if err != nil {
		log.Printf("Error getting Repo: %v", err)
		return fmt.Errorf("error getting github app link")
	}
	if repo == nil {
		log.Printf("Repo not found: Org: %v | repo: %v", orgId, diggerRepoName)
		return fmt.Errorf("Repo not found: Org: %v | repo: %v", orgId, diggerRepoName)
	}

	_, token, err := utils.GetGithubService(gh, installationId, repoFullName, repoOwner, repoName)
	if err != nil {
		log.Printf("Error getting github service: %v", err)
		return fmt.Errorf("error getting github service")
	}

	var isMainBranch bool
	if strings.HasSuffix(ref, defaultBranch) {
		isMainBranch = true
	} else {
		isMainBranch = false
	}

	err = utils.CloneGitRepoAndDoAction(cloneURL, defaultBranch, *token, func(dir string) error {
		config, err := dg_configuration.LoadDiggerConfigYaml(dir, true, nil)
		if err != nil {
			log.Printf("ERROR load digger.yml: %v", err)
			return fmt.Errorf("error loading digger.yml %v", err)
		}
		models.DB.UpdateRepoDiggerConfig(link.OrganizationID, *config, repo, isMainBranch)
		return nil
	})
	if err != nil {
		return fmt.Errorf("error while cloning repo: %v", err)
	}

	return nil
}

//func handlePullRequestEvent(gh utils.GithubClientProvider, payload *github.PullRequestEvent, ciBackendProvider ci_backends.CiBackendProvider) error {
//	installationId := *payload.Installation.ID
//	repoName := *payload.Repo.Name
//	repoOwner := *payload.Repo.Owner.Login
//	repoFullName := *payload.Repo.FullName
//	cloneURL := *payload.Repo.CloneURL
//	prNumber := *payload.PullRequest.Number
//	isDraft := payload.PullRequest.GetDraft()
//	commitSha := payload.PullRequest.Head.GetSHA()
//	branch := payload.PullRequest.Head.GetRef()
//
//	link, err := models.DB.GetGithubAppInstallationLink(installationId)
//	if err != nil {
//		log.Printf("Error getting GetGithubAppInstallationLink: %v", err)
//		return fmt.Errorf("error getting github app link")
//	}
//	organisationId := link.OrganizationID
//
//	diggerYmlStr, ghService, config, projectsGraph, _, _, err := getDiggerConfigForPR(gh, installationId, repoFullName, repoOwner, repoName, cloneURL, prNumber)
//	if err != nil {
//		ghService, _, err := utils.GetGithubService(gh, installationId, repoFullName, repoOwner, repoName)
//		if err != nil {
//			log.Printf("GetGithubService error: %v", err)
//			return fmt.Errorf("error getting ghService to post error comment")
//		}
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: Could not load digger config, error: %v", err))
//		log.Printf("getDiggerConfigForPR error: %v", err)
//		return fmt.Errorf("error getting digger config")
//	}
//
//	impactedProjects, impactedProjectsSourceMapping, _, err := dg_github.ProcessGitHubPullRequestEvent(payload, config, projectsGraph, ghService)
//	if err != nil {
//		log.Printf("Error processing event: %v", err)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: Error processing event: %v", err))
//		return fmt.Errorf("error processing event")
//	}
//
//	jobsForImpactedProjects, _, err := dg_github.ConvertGithubPullRequestEventToJobs(payload, impactedProjects, nil, *config)
//	if err != nil {
//		log.Printf("Error converting event to jobsForImpactedProjects: %v", err)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: Error converting event to jobsForImpactedProjects: %v", err))
//		return fmt.Errorf("error converting event to jobsForImpactedProjects")
//	}
//
//	if len(jobsForImpactedProjects) == 0 {
//		// do not report if no projects are impacted to minimise noise in the PR thread
//		// TODO use status checks instead: https://github.com/diggerhq/digger/issues/1135
//		log.Printf("No projects impacted; not starting any jobs")
//		// This one is for aggregate reporting
//		err = utils.SetPRStatusForJobs(ghService, prNumber, jobsForImpactedProjects)
//		return nil
//	}
//
//	diggerCommand, err := orchestrator_scheduler.GetCommandFromJob(jobsForImpactedProjects[0])
//	if err != nil {
//		log.Printf("could not determine digger command from job: %v", jobsForImpactedProjects[0].Commands)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: could not determine digger command from job: %v", err))
//		return fmt.Errorf("unkown digger command in comment %v", err)
//	}
//
//	if *diggerCommand == orchestrator_scheduler.DiggerCommandNoop {
//		log.Printf("job is of type noop, no actions top perform")
//		return nil
//	}
//
//	// perform locking/unlocking in backend
//	//if config.PrLocks {
//	//	for _, project := range impactedProjects {
//	//		prLock := dg_locking.PullRequestLock{
//	//			InternalLock: locking.BackendDBLock{
//	//				OrgId: organisationId,
//	//			},
//	//			CIService:        ghService,
//	//			Reporter:         comment_updater.NoopReporter{},
//	//			ProjectName:      project.Name,
//	//			ProjectNamespace: repoFullName,
//	//			PrNumber:         prNumber,
//	//		}
//	//		err = dg_locking.PerformLockingActionFromCommand(prLock, *diggerCommand)
//	//		if err != nil {
//	//			utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: Failed perform lock action on project: %v %v", project.Name, err))
//	//			return fmt.Errorf("failed to perform lock action on project: %v, %v", project.Name, err)
//	//		}
//	//	}
//	//}
//
//	// if commands are locking or unlocking we don't need to trigger any jobs
//	if *diggerCommand == orchestrator_scheduler.DiggerCommandUnlock ||
//		*diggerCommand == orchestrator_scheduler.DiggerCommandLock {
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":white_check_mark: Command %v completed successfully", *diggerCommand))
//		return nil
//	}
//
//	if !config.AllowDraftPRs && isDraft {
//		log.Printf("Draft PRs are disabled, skipping PR: %v", prNumber)
//		return nil
//	}
//
//	commentReporter, err := utils.InitCommentReporter(ghService, prNumber, ":construction_worker: Digger starting...")
//	if err != nil {
//		log.Printf("Error initializing comment reporter: %v", err)
//		return fmt.Errorf("error initializing comment reporter")
//	}
//
//	err = utils.ReportInitialJobsStatus(commentReporter, jobsForImpactedProjects)
//	if err != nil {
//		log.Printf("Failed to comment initial status for jobs: %v", err)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: Failed to comment initial status for jobs: %v", err))
//		return fmt.Errorf("failed to comment initial status for jobs")
//	}
//
//	err = utils.SetPRStatusForJobs(ghService, prNumber, jobsForImpactedProjects)
//	if err != nil {
//		log.Printf("error setting status for PR: %v", err)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: error setting status for PR: %v", err))
//		fmt.Errorf("error setting status for PR: %v", err)
//	}
//
//	impactedProjectsMap := make(map[string]dg_configuration.Project)
//	for _, p := range impactedProjects {
//		impactedProjectsMap[p.Name] = p
//	}
//
//	impactedJobsMap := make(map[string]orchestrator_scheduler.Job)
//	for _, j := range jobsForImpactedProjects {
//		impactedJobsMap[j.ProjectName] = j
//	}
//
//	commentId, err := strconv.ParseInt(commentReporter.CommentId, 10, 64)
//	if err != nil {
//		log.Printf("strconv.ParseInt error: %v", err)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: could not handle commentId: %v", err))
//	}
//	batchId, _, err := utils.ConvertJobsToDiggerJobs(*diggerCommand, models2.DiggerVCSGithub, organisationId, impactedJobsMap, impactedProjectsMap, projectsGraph, installationId, branch, prNumber, repoOwner, repoName, repoFullName, commitSha, commentId, diggerYmlStr, 0)
//	if err != nil {
//		log.Printf("ConvertJobsToDiggerJobs error: %v", err)
//		utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: ConvertJobsToDiggerJobs error: %v", err))
//		return fmt.Errorf("error converting jobs")
//	}
//
//	if config.CommentRenderMode == dg_configuration.CommentRenderModeGroupByModule {
//		sourceDetails, err := comment_updater.PostInitialSourceComments(ghService, prNumber, impactedProjectsSourceMapping)
//		if err != nil {
//			log.Printf("PostInitialSourceComments error: %v", err)
//			utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: PostInitialSourceComments error: %v", err))
//			return fmt.Errorf("error posting initial comments")
//		}
//		batch, err := models.DB.GetDiggerBatch(batchId)
//		if err != nil {
//			log.Printf("GetDiggerBatch error: %v", err)
//			utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: PostInitialSourceComments error: %v", err))
//			return fmt.Errorf("error getting digger batch")
//		}
//		batch.SourceDetails, err = json.Marshal(sourceDetails)
//		if err != nil {
//			log.Printf("sourceDetails, json Marshal error: %v", err)
//			utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: json Marshal error: %v", err))
//			return fmt.Errorf("error marshalling sourceDetails")
//		}
//		err = models.DB.UpdateDiggerBatch(batch)
//		if err != nil {
//			log.Printf("UpdateDiggerBatch error: %v", err)
//			utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: UpdateDiggerBatch error: %v", err))
//			return fmt.Errorf("error updating digger batch")
//		}
//	}
//
//	segment.Track(strconv.Itoa(int(organisationId)), "backend_trigger_job")
//
//	//ciBackend, err := ciBackendProvider.GetCiBackend(
//	//	ci_backends.CiBackendOptions{
//	//		GithubClientProvider: gh,
//	//		GithubInstallationId: installationId,
//	//		RepoName:             repoName,
//	//		RepoOwner:            repoOwner,
//	//		RepoFullName:         repoFullName,
//	//	},
//	//)
//	//if err != nil {
//	//	log.Printf("GetCiBackend error: %v", err)
//	//	utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: GetCiBackend error: %v", err))
//	//	return fmt.Errorf("error fetching ci backed %v", err)
//	//}
//	//
//	//err = TriggerDiggerJobs(ciBackend, repoFullName, repoOwner, repoName, batchId, prNumber, ghService, gh)
//	//if err != nil {
//	//	log.Printf("TriggerDiggerJobs error: %v", err)
//	//	utils.InitCommentReporter(ghService, prNumber, fmt.Sprintf(":x: TriggerDiggerJobs error: %v", err))
//	//	return fmt.Errorf("error triggerring Digger Jobs")
//	//}
//
//	return nil
//}

func getDiggerConfigForBranch(gh utils.GithubClientProvider, installationId int64, repoFullName string, repoOwner string, repoName string, cloneUrl string, branch string, prNumber int) (string, *dg_github.GithubService, *dg_configuration.DiggerConfig, graph.Graph[string, dg_configuration.Project], error) {
	ghService, token, err := utils.GetGithubService(gh, installationId, repoFullName, repoOwner, repoName)
	if err != nil {
		log.Printf("Error getting github service: %v", err)
		return "", nil, nil, nil, fmt.Errorf("error getting github service")
	}

	var config *dg_configuration.DiggerConfig
	var diggerYmlStr string
	var dependencyGraph graph.Graph[string, dg_configuration.Project]

	changedFiles, err := ghService.GetChangedFiles(prNumber)
	if err != nil {
		log.Printf("Error getting changed files: %v", err)
		return "", nil, nil, nil, fmt.Errorf("error getting changed files")
	}
	err = utils.CloneGitRepoAndDoAction(cloneUrl, branch, *token, func(dir string) error {
		diggerYmlBytes, err := os.ReadFile(path.Join(dir, "digger.yml"))
		diggerYmlStr = string(diggerYmlBytes)
		config, _, dependencyGraph, err = dg_configuration.LoadDiggerConfig(dir, true, changedFiles)
		if err != nil {
			log.Printf("Error loading digger config: %v", err)
			return err
		}
		return nil
	})
	if err != nil {
		log.Printf("Error cloning and loading config: %v", err)
		return "", nil, nil, nil, fmt.Errorf("error cloning and loading config")
	}

	log.Printf("Digger config loadded successfully\n")
	return diggerYmlStr, ghService, config, dependencyGraph, nil
}

// TODO: Refactor this func to receive ghService as input
func getDiggerConfigForPR(gh utils.GithubClientProvider, installationId int64, repoFullName string, repoOwner string, repoName string, cloneUrl string, prNumber int) (string, *dg_github.GithubService, *dg_configuration.DiggerConfig, graph.Graph[string, dg_configuration.Project], *string, *string, error) {
	ghService, _, err := utils.GetGithubService(gh, installationId, repoFullName, repoOwner, repoName)
	if err != nil {
		log.Printf("Error getting github service: %v", err)
		return "", nil, nil, nil, nil, nil, fmt.Errorf("error getting github service")
	}

	var prBranch string
	prBranch, prCommitSha, err := ghService.GetBranchName(prNumber)
	if err != nil {
		log.Printf("Error getting branch name: %v", err)
		return "", nil, nil, nil, nil, nil, fmt.Errorf("error getting branch name")
	}

	diggerYmlStr, ghService, config, dependencyGraph, err := getDiggerConfigForBranch(gh, installationId, repoFullName, repoOwner, repoName, cloneUrl, prBranch, prNumber)
	if err != nil {
		log.Printf("Error loading digger.yml: %v", err)
		return "", nil, nil, nil, nil, nil, fmt.Errorf("error loading digger.yml")
	}

	log.Printf("Digger config loadded successfully\n")
	return diggerYmlStr, ghService, config, dependencyGraph, &prBranch, &prCommitSha, nil
}

func GetRepoByInstllationId(installationId int64, repoOwner string, repoName string) (*model.Repo, error) {
	link, err := models.DB.GetGithubAppInstallationLink(installationId)
	if err != nil {
		log.Printf("Error getting GetGithubAppInstallationLink: %v", err)
		return nil, fmt.Errorf("error getting github app link")
	}

	if link == nil {
		log.Printf("Failed to find GithubAppInstallationLink for installationId: %v", installationId)
		return nil, fmt.Errorf("error getting github app installation link")
	}

	diggerRepoName := repoOwner + "-" + repoName
	repo, err := models.DB.GetRepo(link.OrganizationID, diggerRepoName)
	return repo, nil
}

func getBatchType(jobs []orchestrator_scheduler.Job) orchestrator_scheduler.DiggerBatchType {
	allJobsContainApply := lo.EveryBy(jobs, func(job orchestrator_scheduler.Job) bool {
		return lo.Contains(job.Commands, "digger apply")
	})
	if allJobsContainApply == true {
		return orchestrator_scheduler.BatchTypeApply
	} else {
		return orchestrator_scheduler.BatchTypePlan
	}
}

func (d DiggerController) GithubAppCallbackPage(c *gin.Context) {
	installationId := c.Request.URL.Query()["installation_id"][0]
	//setupAction := c.Request.URL.Query()["setup_action"][0]
	code := c.Request.URL.Query()["code"][0]
	clientId := os.Getenv("GITHUB_APP_CLIENT_ID")
	clientSecret := os.Getenv("GITHUB_APP_CLIENT_SECRET")

	installationId64, err := strconv.ParseInt(installationId, 10, 64)
	if err != nil {
		log.Printf("err: %v", err)
		c.String(http.StatusInternalServerError, "Failed to parse installation_id.")
		return
	}

	result, err := validateGithubCallback(d.GithubClientProvider, clientId, clientSecret, code, installationId64)
	if !result {
		log.Printf("Failed to validated installation id, %v\n", err)
		c.String(http.StatusInternalServerError, "Failed to validate installation_id.")
		return
	}

	orgId := c.GetString(middleware.ORGANISATION_ID_KEY)
	org, err := models.DB.GetOrganisationById(orgId)
	if err != nil {
		log.Printf("Error fetching organisation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error fetching organisation"})
		return
	}

	_, err = models.DB.CreateGithubInstallationLink(org, installationId64)
	if err != nil {
		log.Printf("Error saving CreateGithubInstallationLink to database: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error updating GitHub installation"})
		return
	}

	c.HTML(http.StatusOK, "github_success.tmpl", gin.H{})
}

func (d DiggerController) GithubReposPage(c *gin.Context) {
	orgId, exists := c.Get(middleware.ORGANISATION_ID_KEY)
	if !exists {
		log.Printf("Organisation ID not found in context")
		c.String(http.StatusForbidden, "Not allowed to access this resource")
		return
	}

	link, err := models.DB.GetGithubInstallationLinkForOrg(orgId)
	if err != nil {
		log.Printf("GetGithubInstallationLinkForOrg error: %v\n", err)
		c.String(http.StatusForbidden, "Failed to find any GitHub installations for this org")
		return
	}

	installations, err := models.DB.GetGithubAppInstallations(link.GithubInstallationID)
	if err != nil {
		log.Printf("GetGithubAppInstallations error: %v\n", err)
		c.String(http.StatusForbidden, "Failed to find any GitHub installations for this org")
		return
	}

	if len(installations) == 0 {
		c.String(http.StatusForbidden, "Failed to find any GitHub installations for this org")
		return
	}

	gh := d.GithubClientProvider
	client, _, err := gh.Get(installations[0].GithubAppID, installations[0].GithubInstallationID)
	if err != nil {
		log.Printf("failed to create github client, %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error creating GitHub client"})
		return
	}

	opts := &github.ListOptions{}
	repos, _, err := client.Apps.ListRepos(context.Background(), opts)
	if err != nil {
		log.Printf("GetGithubAppInstallations error: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list GitHub repos."})
		return
	}
	c.HTML(http.StatusOK, "github_repos.tmpl", gin.H{"Repos": repos.Repositories})
}

// why this validation is needed: https://roadie.io/blog/avoid-leaking-github-org-data/
// validation based on https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app , step 3
func validateGithubCallback(githubClientProvider utils.GithubClientProvider, clientId string, clientSecret string, code string, installationId int64) (bool, error) {
	ctx := context.Background()
	type OAuthAccessResponse struct {
		AccessToken string `json:"access_token"`
	}
	httpClient := http.Client{}

	githubHostname := getGithubHostname()
	reqURL := fmt.Sprintf("https://%v/login/oauth/access_token?client_id=%s&client_secret=%s&code=%s", githubHostname, clientId, clientSecret, code)
	req, err := http.NewRequest(http.MethodPost, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("could not create HTTP request: %v\n", err)
	}
	req.Header.Set("accept", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("request to login/oauth/access_token failed: %v\n", err)
	}

	if err != nil {
		return false, fmt.Errorf("Failed to read response's body: %v\n", err)
	}

	var t OAuthAccessResponse
	if err := json.NewDecoder(res.Body).Decode(&t); err != nil {
		return false, fmt.Errorf("could not parse JSON response: %v\n", err)
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: t.AccessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	//tc := &http.Client{
	//	Transport: &oauth2.Transport{
	//		Base:   httpClient.Transport,
	//		Source: oauth2.ReuseTokenSource(nil, ts),
	//	},
	//}

	client, err := githubClientProvider.NewClient(tc)
	if err != nil {
		log.Printf("could create github client: %v", err)
		return false, fmt.Errorf("could not create github client: %v", err)
	}

	installationIdMatch := false
	// list all installations for the user
	installations, _, err := client.Apps.ListUserInstallations(ctx, nil)
	if err != nil {
		log.Printf("could not retrieve installations: %v", err)
		return false, fmt.Errorf("could not retrieve installations: %v", installationId)
	}
	log.Printf("installations %v", installations)
	for _, v := range installations {
		log.Printf("installation id: %v\n", *v.ID)
		if *v.ID == installationId {
			installationIdMatch = true
		}
	}
	if !installationIdMatch {
		return false, fmt.Errorf("InstallationId %v doesn't match any id for specified user\n", installationId)
	}
	return true, nil
}