package account

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/slice"

	"github.com/argoproj/argo-cd/pkg/apiclient/account"
	"github.com/argoproj/argo-cd/server/rbacpolicy"
	"github.com/argoproj/argo-cd/util/password"
	"github.com/argoproj/argo-cd/util/rbac"
	"github.com/argoproj/argo-cd/util/session"
	"github.com/argoproj/argo-cd/util/settings"
)

// Server provides a Session service
type Server struct {
	sessionMgr  *session.SessionManager
	settingsMgr *settings.SettingsManager
	enf         *rbac.Enforcer
}

// NewServer returns a new instance of the Session service
func NewServer(sessionMgr *session.SessionManager, settingsMgr *settings.SettingsManager, enf *rbac.Enforcer) *Server {
	return &Server{sessionMgr, settingsMgr, enf}
}

// UpdatePassword updates the password of the currently authenticated account or the account specified in the request.
func (s *Server) UpdatePassword(ctx context.Context, q *account.UpdatePasswordRequest) (*account.UpdatePasswordResponse, error) {
	username := session.Sub(ctx)
	if rbacpolicy.IsProjectSubject(username) || session.Iss(ctx) != session.SessionManagerClaimsIssuer {
		return nil, status.Errorf(codes.InvalidArgument, "password can only be changed for local users, not user %q", username)
	}

	err := s.sessionMgr.VerifyUsernamePassword(username, q.CurrentPassword)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "current password does not match")
	}

	updatedUsername := username
	if q.Name != "" && q.Name != username {
		if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceAccounts, rbacpolicy.ActionUpdate, q.Name); err != nil {
			return nil, err
		}
		updatedUsername = q.Name
	}

	hashedPassword, err := password.HashPassword(q.NewPassword)
	if err != nil {
		return nil, err
	}

	err = s.settingsMgr.UpdateAccount(updatedUsername, func(acc *settings.Account) error {
		acc.PasswordHash = hashedPassword
		now := time.Now().UTC()
		acc.PasswordMtime = &now
		return nil
	})

	if err != nil {
		return nil, err
	}

	if updatedUsername == username {
		log.Infof("user '%s' updated password", username)
	} else {
		log.Infof("user '%s' updated password of user '%s'", username, updatedUsername)
	}
	return &account.UpdatePasswordResponse{}, nil

}

func (s *Server) CanI(ctx context.Context, r *account.CanIRequest) (*account.CanIResponse, error) {
	if !slice.ContainsString(rbacpolicy.Actions, r.Action, nil) {
		return nil, status.Errorf(codes.InvalidArgument, "%v does not contain %s", rbacpolicy.Actions, r.Action)
	}
	if !slice.ContainsString(rbacpolicy.Resources, r.Resource, nil) {
		return nil, status.Errorf(codes.InvalidArgument, "%v does not contain %s", rbacpolicy.Resources, r.Resource)
	}
	ok := s.enf.Enforce(ctx.Value("claims"), r.Resource, r.Action, r.Subresource)
	if ok {
		return &account.CanIResponse{Value: "yes"}, nil
	} else {
		return &account.CanIResponse{Value: "no"}, nil
	}
}

func toApiAccount(name string, a settings.Account) *account.Account {
	var capabilities []string
	for _, c := range a.Capabilities {
		capabilities = append(capabilities, string(c))
	}
	var tokens []*account.Token
	for _, t := range a.Tokens {
		tokens = append(tokens, &account.Token{Id: t.ID, ExpiresAt: t.ExpiresAt, IssuedAt: t.IssuedAt})
	}
	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].IssuedAt > tokens[j].IssuedAt
	})
	return &account.Account{
		Name:         name,
		Enabled:      a.Enabled,
		Capabilities: capabilities,
		Tokens:       tokens,
	}
}

func (s *Server) ensureHasAccountPermission(ctx context.Context, action string, account string) error {
	// account has always has access to itself
	if session.Sub(ctx) == account && session.Iss(ctx) == session.SessionManagerClaimsIssuer {
		return nil
	}
	if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceAccounts, action, account); err != nil {
		return err
	}
	return nil
}

func (s *Server) ListAccounts(ctx context.Context, r *account.ListAccountRequest) (*account.AccountsList, error) {
	resp := account.AccountsList{}
	accounts, err := s.settingsMgr.GetAccounts()
	if err != nil {
		return nil, err
	}
	for name, a := range accounts {
		if err := s.ensureHasAccountPermission(ctx, rbacpolicy.ActionGet, name); err == nil {
			resp.Items = append(resp.Items, toApiAccount(name, a))
		}
	}
	sort.Slice(resp.Items, func(i, j int) bool {
		return resp.Items[i].Name < resp.Items[j].Name
	})
	return &resp, nil
}

func (s *Server) GetAccount(ctx context.Context, r *account.GetAccountRequest) (*account.Account, error) {
	if err := s.ensureHasAccountPermission(ctx, rbacpolicy.ActionGet, r.Name); err != nil {
		return nil, err
	}
	a, err := s.settingsMgr.GetAccount(r.Name)
	if err != nil {
		return nil, err
	}
	return toApiAccount(r.Name, *a), nil
}

func (s *Server) CreateToken(ctx context.Context, r *account.CreateTokenRequest) (*account.CreateTokenResponse, error) {
	if err := s.ensureHasAccountPermission(ctx, rbacpolicy.ActionUpdate, r.Name); err != nil {
		return nil, err
	}

	var tokenString string
	err := s.settingsMgr.UpdateAccount(r.Name, func(account *settings.Account) error {
		if !account.HasCapability(settings.AccountCapabilityApiKey) {
			return fmt.Errorf("account '%s' does not have %s capability", r.Name, settings.AccountCapabilityApiKey)
		}

		var err error
		uniqueId, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		id := uniqueId.String()
		now := time.Now()
		tokenString, err = s.sessionMgr.Create(r.Name, r.ExpiresIn, id)
		if err != nil {
			return err
		}

		var expiresAt int64
		if r.ExpiresIn > 0 {
			expiresAt = now.Add(time.Duration(r.ExpiresIn) * time.Second).Unix()
		}
		account.Tokens = append(account.Tokens, settings.Token{
			ID:        id,
			IssuedAt:  now.Unix(),
			ExpiresAt: expiresAt,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &account.CreateTokenResponse{Token: tokenString}, nil
}

func (s *Server) DeleteToken(ctx context.Context, r *account.DeleteTokenRequest) (*account.EmptyResponse, error) {
	if err := s.ensureHasAccountPermission(ctx, rbacpolicy.ActionUpdate, r.Name); err != nil {
		return nil, err
	}

	err := s.settingsMgr.UpdateAccount(r.Name, func(account *settings.Account) error {
		if index := account.TokenIndex(r.Id); index > -1 {
			account.Tokens = append(account.Tokens[:index], account.Tokens[index+1:]...)
			return nil
		}
		return status.Errorf(codes.NotFound, "token with id '%s'does not exist", r.Id)
	})
	if err != nil {
		return nil, err
	}
	return &account.EmptyResponse{}, nil
}
