package repo

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"forgejo.org/models/perm"
	access_model "forgejo.org/models/perm/access"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unit"
	"forgejo.org/modules/annex"
	"forgejo.org/modules/graceful"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
	services_context "forgejo.org/services/context"
)

type p2phttpRecordType struct {
	CancelFunc func()
	LastUsed   time.Time
	Port       string
}

var p2phttpRecords = make(map[string]*p2phttpRecordType)

// AnnexP2PHTTP implements git-annex smart HTTP support by delegating to git annex p2phttp
func AnnexP2PHTTP(ctx *services_context.Context) {
	uuid := ctx.Params(":uuid")
	repoPath, err := annex.UUID2RepoPath(uuid)
	if err != nil {
		ctx.PlainText(http.StatusNotFound, "Repository not found")
		return
	}

	parts := strings.Split(repoPath, "/")
	repoName := strings.TrimSuffix(parts[len(parts)-1], ".git")
	owner := parts[len(parts)-2]
	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		ctx.PlainText(http.StatusNotFound, "Repository not found")
		return
	}

	p, err := access_model.GetUserRepoPermission(ctx, repo, ctx.Doer)
	if err != nil {
		ctx.ServerError("GetUserRepoPermission", err)
		return
	}

	if !(ctx.Req.Method == "GET" && p.CanAccess(perm.AccessModeRead, unit.TypeCode) ||
		ctx.Req.Method == "POST" && p.CanAccess(perm.AccessModeWrite, unit.TypeCode) ||
		ctx.Req.Method == "POST" && strings.HasSuffix(ctx.Req.URL.Path, "/checkpresent") && p.CanAccess(perm.AccessModeRead, unit.TypeCode) ||
		ctx.Req.Method == "POST" && strings.HasSuffix(ctx.Req.URL.Path, "/keeplocked") ||
		ctx.Req.Method == "POST" && strings.HasSuffix(ctx.Req.URL.Path, "/lockcontent")) {
		// GET requests require at least read access; POST requests for
		// anything but checkpresent, lockcontent, and keeplocked
		// require write permissions; POST requests for checkpresent
		// only require read permissions, as it really is just a read.
		// POST requests for lockcontent and keeplocked require no
		// authentication at all, as is also the case for the
		// authentication in the git-annex-p2phttp server. See
		// https://git-annex.branchable.com/bugs/p2phttp__58___drop_difference_wideopen_unauth-readonly/
		// for reasoning.
		ctx.Resp.WriteHeader(http.StatusUnauthorized)
		return
	}

	p2phttpRecord, p2phttpProcessExists := p2phttpRecords[uuid]
	if p2phttpProcessExists {
		p2phttpRecord.LastUsed = time.Now()
	} else {
		// Start a new p2phttp process for the requested repository
		// There is a race condition here with the port selection, ideally git annex p2phttp could just listen on a unix socket...
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Error("Failed to listen on a free port: %v", err)
			ctx.Resp.WriteHeader(http.StatusInternalServerError)
			return
		}
		hopefullyFreePort := strings.SplitN(lis.Addr().String(), ":", 2)[1]
		lis.Close()
		p2phttpCtx, p2phttpCtxCancel := context.WithCancel(context.Background())
		go func(ctx context.Context) {
			cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "annex", "p2phttp", "-J2", "--bind", "127.0.0.1", "--wideopen", "--port", hopefullyFreePort)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Pdeathsig: syscall.SIGINT,
			}
			cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME="+setting.AppName,
				"GIT_AUTHOR_EMAIL="+setting.RunUser+"@"+setting.Domain,
			)
			_ = cmd.Run()
		}(p2phttpCtx)
		graceful.GetManager().RunAtTerminate(p2phttpCtxCancel)

		// Wait for the p2phttp server to get ready
		start := time.Now()
		sleepDuration := 1 * time.Millisecond
		for {
			if time.Since(start) > 5*time.Second {
				p2phttpCtxCancel()
				log.Error("Failed to start the p2phttp server in a reasonable amount of time")
				ctx.Resp.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, err := net.Dial("tcp", "127.0.0.1:"+hopefullyFreePort)
			if err == nil {
				conn.Close()
				break
			}
			time.Sleep(sleepDuration)
			sleepDuration *= 2
			if sleepDuration > 1*time.Second {
				sleepDuration = 1 * time.Second
			}
		}

		p2phttpRecord = &p2phttpRecordType{CancelFunc: p2phttpCtxCancel, LastUsed: time.Now(), Port: hopefullyFreePort}
		p2phttpRecords[uuid] = p2phttpRecord
	}

	// Cleanup p2phttp processes that haven't been used for a while
	for uuid, record := range p2phttpRecords {
		if time.Since(record.LastUsed) > 5*time.Minute {
			record.CancelFunc()
			delete(p2phttpRecords, uuid)
		}
	}

	url, err := url.Parse("http://127.0.0.1:" + p2phttpRecord.Port + strings.TrimPrefix(ctx.Req.RequestURI, "/git-annex-p2phttp"))
	if err != nil {
		log.Error("Failed to parse URL: %v", err)
		ctx.Resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	proxy := httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL = url
		},
	}
	proxy.ServeHTTP(ctx.Resp, ctx.Req)
}
