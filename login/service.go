package login

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"time"

	name "github.com/fabric8-services/fabric8-auth/account"
	account "github.com/fabric8-services/fabric8-auth/account/repository"
	"github.com/fabric8-services/fabric8-auth/app"
	"github.com/fabric8-services/fabric8-auth/application"
	"github.com/fabric8-services/fabric8-auth/application/transaction"
	"github.com/fabric8-services/fabric8-auth/auth"
	"github.com/fabric8-services/fabric8-auth/configuration"
	autherrors "github.com/fabric8-services/fabric8-auth/errors"
	"github.com/fabric8-services/fabric8-auth/jsonapi"
	"github.com/fabric8-services/fabric8-auth/log"
	"github.com/fabric8-services/fabric8-auth/rest"
	"github.com/fabric8-services/fabric8-auth/token"
	keycloaktoken "github.com/fabric8-services/fabric8-auth/token/keycloak"
	"github.com/fabric8-services/fabric8-auth/token/oauth"
	"github.com/fabric8-services/fabric8-auth/token/tokencontext"
	"github.com/goadesign/goa"
	errs "github.com/pkg/errors"
	"github.com/satori/go.uuid"
	"golang.org/x/oauth2"
)

type Configuration interface {
	GetKeycloakEndpointBroker(*goa.RequestData) (string, error)
	GetKeycloakEndpointToken(*goa.RequestData) (string, error)
	GetKeycloakClientID() string
	GetKeycloakSecret() string
	GetKeycloakEndpointUsers(*goa.RequestData) (string, error)
	GetValidRedirectURLs() string
	GetNotApprovedRedirect() string
	GetWITURL() (string, error)
	GetOpenShiftClientApiUrl() string
	GetKeycloakAccountEndpoint(*goa.RequestData) (string, error)
	IsPostgresDeveloperModeEnabled() bool
	GetOSORegistrationAppURL() string
	GetOSORegistrationAppAdminUsername() string
	GetOSORegistrationAppAdminToken() string
	GetOSOClusterByURL(url string) *configuration.OSOCluster
}

// NewKeycloakOAuthProvider creates a new login.Service capable of using keycloak for authorization
func NewKeycloakOAuthProvider(identities account.IdentityRepository, users account.UserRepository, tokenManager token.Manager, app application.Application, keycloakProfileService UserProfileService, keycloakTokenService keycloaktoken.TokenService, osoSubscriptionManager OSOSubscriptionManager) *KeycloakOAuthProvider {
	return &KeycloakOAuthProvider{
		Identities:   identities,
		Users:        users,
		TokenManager: tokenManager,
		App:          app,
		keycloakProfileService: keycloakProfileService,
		keycloakTokenService:   keycloakTokenService,
		osoSubscriptionManager: osoSubscriptionManager,
	}
}

// KeycloakOAuthProvider represents a keycloak IDP
type KeycloakOAuthProvider struct {
	Identities             account.IdentityRepository
	Users                  account.UserRepository
	TokenManager           token.Manager
	App                    application.Application
	keycloakProfileService UserProfileService
	keycloakTokenService   keycloaktoken.TokenService
	osoSubscriptionManager OSOSubscriptionManager
}

// KeycloakOAuthService represents keycloak OAuth service interface
type KeycloakOAuthService interface {
	Login(ctx *app.LoginLoginContext, config oauth.OauthConfig, serviceConfig Configuration) error
	AuthCodeURL(ctx context.Context, redirect *string, apiClient *string, state *string, responseMode *string, request *goa.RequestData, config oauth.OauthConfig, serviceConfig Configuration) (*string, error)
	Exchange(ctx context.Context, code string, config oauth.OauthConfig) (*oauth2.Token, error)
	ExchangeRefreshToken(ctx context.Context, refreshToken string, endpoint string, serviceConfig Configuration) (*token.TokenSet, error)
	AuthCodeCallback(ctx *app.CallbackAuthorizeContext) (*string, error)
	CreateOrUpdateIdentityInDB(ctx context.Context, accessToken string, configuration Configuration) (*account.Identity, bool, error)
	CreateOrUpdateIdentityAndUser(ctx context.Context, referrerURL *url.URL, keycloakToken *oauth2.Token, request *goa.RequestData, serviceConfig Configuration) (*string, *oauth2.Token, error)
}

const (
	apiClientParam = "api_client"
	apiTokenParam  = "api_token"
	tokenJSONParam = "token_json"
)

// Login performs authentication
func (keycloak *KeycloakOAuthProvider) Login(ctx *app.LoginLoginContext, config oauth.OauthConfig, serviceConfig Configuration) error {

	state := ctx.Params.Get("state")
	code := ctx.Params.Get("code")

	log.Debug(ctx, map[string]interface{}{
		"code":  code,
		"state": state,
	}, "login request received")

	if code != "" {
		// After redirect from oauth provider
		log.Debug(ctx, map[string]interface{}{
			"code":  code,
			"state": state,
		}, "Redirected from oauth provider")

		referrerURL, _, err := keycloak.reclaimReferrerAndResponseMode(ctx, state, code)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, err)
		}

		keycloakToken, err := keycloak.Exchange(ctx, code, config)

		if err != nil {
			jsonapi.JSONErrorResponse(ctx, err)
			ctx.ResponseData.Header().Set("Location", referrerURL.String()+"?error="+err.Error())
			return ctx.TemporaryRedirect()
		}

		redirectTo, _, err := keycloak.CreateOrUpdateIdentityAndUser(ctx, referrerURL, keycloakToken, ctx.RequestData, serviceConfig)
		if err != nil {
			jsonapi.JSONErrorResponse(ctx, err)
		}

		if redirectTo != nil {
			ctx.ResponseData.Header().Set("Location", *redirectTo)
			return ctx.TemporaryRedirect()
		}

		ctx.ResponseData.Header().Set("Location", referrerURL.String()+"?error="+err.Error())
		return ctx.TemporaryRedirect()
	}

	// First time access, redirect to oauth provider
	generatedState := uuid.NewV4().String()
	redirectURL, err := keycloak.AuthCodeURL(ctx, ctx.Redirect, ctx.APIClient, &generatedState, nil, ctx.RequestData, config, serviceConfig)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	ctx.ResponseData.Header().Set("Location", *redirectURL)
	return ctx.TemporaryRedirect()
}

// AuthCodeURL is used in authorize action of /api/authorize to get authorization_code
func (keycloak *KeycloakOAuthProvider) AuthCodeURL(ctx context.Context, redirect *string, apiClient *string, state *string, responseMode *string, request *goa.RequestData, config oauth.OauthConfig, serviceConfig Configuration) (*string, error) {
	/* Compute all the configuration urls */
	validRedirectURL := serviceConfig.GetValidRedirectURLs()

	// First time access, redirect to oauth provider
	referrer := request.Header.Get("Referer")
	if redirect == nil {
		if referrer == "" {
			return nil, autherrors.NewBadParameterError("Referer Header and redirect param are both empty. At least one should be specified", redirect).Expected("redirect")
		}
		redirect = &referrer
	}
	// store referrer in a state reference to redirect later
	log.Debug(ctx, map[string]interface{}{
		"referrer": referrer,
		"redirect": redirect,
	}, "Got Request from!")

	redirect, err := keycloak.saveParams(ctx, *redirect, apiClient)
	if err != nil {
		return nil, err
	}

	err = keycloak.saveReferrer(ctx, *state, *redirect, responseMode, validRedirectURL)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"state":         state,
			"referrer":      referrer,
			"redirect":      redirect,
			"response_mode": responseMode,
			"err":           err,
		}, "unable to save the state")
		return nil, err
	}

	redirectTo := config.AuthCodeURL(*state, oauth2.AccessTypeOnline)

	return &redirectTo, err
}

// Exchange exchanges the given code for OAuth2 token with Keycloak
func (keycloak *KeycloakOAuthProvider) Exchange(ctx context.Context, code string, config oauth.OauthConfig) (*oauth2.Token, error) {

	// Exchange the code for a Keycloak token
	keycloakToken, err := config.Exchange(ctx, code)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"code": code,
			"err":  err,
		}, "keycloak exchange operation failed")
		return nil, autherrors.NewUnauthorizedError(err.Error())
	}

	log.Debug(ctx, map[string]interface{}{
		"code": code,
	}, "exchanged code to access token")

	return keycloakToken, nil
}

// ExchangeRefreshToken exchanges refreshToken for OauthToken
func (keycloak *KeycloakOAuthProvider) ExchangeRefreshToken(ctx context.Context, refreshToken string, endpoint string, serviceConfig Configuration) (*token.TokenSet, error) {

	// Load identity for the refresh token
	var identity *account.Identity
	claims, err := keycloak.TokenManager.ParseTokenWithMapClaims(ctx, refreshToken)
	if err != nil {
		return nil, autherrors.NewUnauthorizedError(err.Error())
	}
	sub := claims["sub"]
	if sub == nil {
		return nil, autherrors.NewUnauthorizedError("missing 'sub' claim in the refresh token")
	}
	identityID, err := uuid.FromString(fmt.Sprintf("%s", sub))
	if err != nil {
		return nil, autherrors.NewUnauthorizedError(err.Error())
	}
	err = transaction.Transactional(keycloak.App, func(tr transaction.TransactionalResources) error {
		identity, err = tr.Identities().LoadWithUser(ctx, identityID)
		return err
	})
	if err != nil {
		// That's OK if we didn't find the identity if the token was issued for an API client
		// Just log it and proceed.
		log.Warn(ctx, map[string]interface{}{
			"err": err,
		}, "failed to load identity when refreshing token; it's OK if the token was issued for an API client")
	}
	if identity != nil && identity.User.Deprovisioned {
		log.Warn(ctx, map[string]interface{}{
			"identity_id": identity.ID,
			"user_name":   identity.Username,
		}, "deprovisioned user tried to refresh token")
		return nil, autherrors.NewUnauthorizedError("unauthorized access")
	}

	// Refresh token in Keycloak
	tokeSet, err := keycloak.keycloakTokenService.RefreshToken(ctx, endpoint, serviceConfig.GetKeycloakClientID(), serviceConfig.GetKeycloakSecret(), refreshToken)
	if err != nil {
		if serviceConfig.IsPostgresDeveloperModeEnabled() && identity != nil && reflect.TypeOf(keycloak.keycloakTokenService) == reflect.TypeOf(&keycloaktoken.KeycloakTokenService{}) {
			// If running in dev mode but not in a test then we ignore an error from Keycloak and just generate a refresh token
			generatedToken, err := keycloak.TokenManager.GenerateUserTokenForIdentity(ctx, *identity, false)
			if err != nil {
				return nil, err
			}
			return keycloak.TokenManager.ConvertToken(*generatedToken)
		}
		return nil, err
	}

	// Generate token based on the Keycloak token
	oauthToken := keycloak.TokenManager.ConvertTokenSet(*tokeSet)
	generatedToken, err := keycloak.TokenManager.GenerateUserToken(ctx, *oauthToken, identity)
	if err != nil {
		return nil, err
	}

	return keycloak.TokenManager.ConvertToken(*generatedToken)
}

// CreateOrUpdateIdentityAndUser creates or updates user and identity, checks whether the user is approved,
// encodes the token and returns final URL to which we are supposed to redirect
func (keycloak *KeycloakOAuthProvider) CreateOrUpdateIdentityAndUser(ctx context.Context, referrerURL *url.URL, keycloakToken *oauth2.Token, request *goa.RequestData, config Configuration) (*string, *oauth2.Token, error) {
	witURL, err := config.GetWITURL()
	if err != nil {
		return nil, nil, autherrors.NewInternalError(ctx, err)
	}

	apiClient := referrerURL.Query().Get(apiClientParam)

	identity, newUser, err := keycloak.CreateOrUpdateIdentityInDB(ctx, keycloakToken.AccessToken, config)

	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err": err,
		}, "failed to create a user and keycloak identity ")
		switch err.(type) {
		case autherrors.UnauthorizedError:
			if apiClient != "" {
				// Return the api token
				userToken, err := keycloak.TokenManager.GenerateUserToken(ctx, *keycloakToken, nil)
				if err != nil {
					log.Error(ctx, map[string]interface{}{"err": err}, "failed to generate token")
					return nil, nil, err
				}
				err = encodeToken(ctx, referrerURL, userToken, apiClient)
				if err != nil {
					log.Error(ctx, map[string]interface{}{"err": err}, "failed to encode token")
					return nil, nil, err
				}
				log.Info(ctx, map[string]interface{}{
					"referrerURL": referrerURL.String(),
					"api_client":  apiClient,
				}, "return api token for unapproved user")
				redirectTo := referrerURL.String()
				return &redirectTo, userToken, nil
			}

			userNotApprovedRedirectURL := config.GetNotApprovedRedirect()
			if userNotApprovedRedirectURL != "" {
				status, err := keycloak.osoSubscriptionManager.LoadOSOSubscriptionStatus(ctx, config, *keycloakToken)
				if err != nil {
					// Not critical. Just log the error and proceed
					log.Error(ctx, map[string]interface{}{"err": err}, "failed to load OSO subscription status")
				}
				userNotApprovedRedirectURL, err := rest.AddParam(userNotApprovedRedirectURL, "status", status)
				if err != nil {
					log.Error(ctx, map[string]interface{}{"err": err}, "failed to add a status param to the redirect URL")
					return nil, nil, err
				}
				log.Debug(ctx, map[string]interface{}{
					"user_not_approved_redirect_url": userNotApprovedRedirectURL,
				}, "user not approved; redirecting to registration app")
				return &userNotApprovedRedirectURL, nil, nil
			}
			return nil, nil, autherrors.NewUnauthorizedError(err.Error())
		}
		return nil, nil, err
	}

	if identity.User.Deprovisioned {
		log.Warn(ctx, map[string]interface{}{
			"identity_id": identity.ID,
			"user_name":   identity.Username,
		}, "deprovisioned user tried to login")
		return nil, nil, autherrors.NewUnauthorizedError("unauthorized access")
	}

	log.Debug(ctx, map[string]interface{}{
		"referrerURL": referrerURL.String(),
		"user_name":   identity.Username,
	}, "local user created/updated")

	// Generate a new token instead of using the original Keycloak token
	userToken, err := keycloak.TokenManager.GenerateUserToken(ctx, *keycloakToken, identity)
	if err != nil {
		log.Error(ctx, map[string]interface{}{"err": err, "identity_id": identity.ID.String()}, "failed to generate token")
		return nil, nil, err
	}

	_, err = keycloak.synchronizeAuthToKeycloak(ctx, request, keycloakToken, config, identity)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"identity_id": identity.ID,
			"username":    identity.Username,
		}, "unable to synchronize user from auth to keycloak ")

		// don't wish to cause a login error if something goes wrong here
	}

	// new user for WIT
	if newUser {
		err = keycloak.App.WITService().CreateUser(ctx, identity, identity.ID.String())
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":         err,
				"identity_id": identity.ID,
				"username":    identity.Username,
				"wit_url":     witURL,
			}, "unable to create user in WIT ")
			// let's carry on instead of erroring out
		}
	} else {
		err = keycloak.updateWITUser(ctx, identity, witURL, identity.ID.String())
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"identity_id": identity.ID,
				"username":    identity.Username,
				"err":         err,
				"wit_url":     witURL,
			}, "unable to update user in WIT ")
			// let's carry on instead of erroring out
		}
	}

	err = encodeToken(ctx, referrerURL, userToken, apiClient)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err": err,
		}, "failed to encode token")
		redirectTo := referrerURL.String() + err.Error()
		return &redirectTo, nil, autherrors.NewInternalError(ctx, err)
	}
	log.Debug(ctx, map[string]interface{}{
		"referrerURL": referrerURL.String(),
		"user_name":   identity.Username,
	}, "token encoded")

	redirectTo := referrerURL.String()
	return &redirectTo, userToken, nil
}

func (keycloak *KeycloakOAuthProvider) updateUserInKeycloak(ctx context.Context, request *goa.RequestData, keycloakUser KeycloakUserRequest, config Configuration, identity *account.Identity) error {
	tokenEndpoint, err := config.GetKeycloakEndpointToken(request)
	if err != nil {
		return autherrors.NewInternalError(ctx, err)
	}
	protectedAccessToken, err := auth.GetProtectedAPIToken(ctx, tokenEndpoint, config.GetKeycloakClientID(), config.GetKeycloakSecret())
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"keycloak_client_id": config.GetKeycloakClientID(),
			"token_endpoint":     tokenEndpoint,
			"err":                err,
		}, "error generating PAT")
		return err
	}

	if protectedAccessToken != "" {
		// try hitting the admin user endpoint only if getting a PAT
		// was successful.

		usersEndpoint, err := config.GetKeycloakEndpointUsers(request)

		// not using userProfileService.Update() because it needs a user token
		// and here we don't have one.
		keycloakUserID, _, err := keycloak.keycloakProfileService.CreateOrUpdate(ctx, &keycloakUser, protectedAccessToken, usersEndpoint)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err": err,
			}, "failed to update user in keycloak")
			return err
		} else {
			log.Info(ctx, map[string]interface{}{
				"keycloak_user_id": *keycloakUserID,
			}, "successfully updated user in keycloak")
			return nil
		}
	}
	return autherrors.NewInternalErrorFromString(ctx, "couldn't update profile because PAT wasn't generated")
}

func (keycloak *KeycloakOAuthProvider) synchronizeAuthToKeycloak(ctx context.Context, request *goa.RequestData, keycloakToken *oauth2.Token, config Configuration, identity *account.Identity) (*oauth2.Token, error) {
	// Sync from auth db to keycloak.

	accountAPIEndpoint, err := config.GetKeycloakAccountEndpoint(request)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":       err,
			"user_name": identity.Username,
		}, "error getting account endpoint")
		return nil, err
	}

	claims, err := keycloak.TokenManager.ParseToken(ctx, keycloakToken.AccessToken)
	tokenRefreshNeeded := !keycloak.equalsTokenClaims(ctx, claims, *identity)
	log.Info(ctx, map[string]interface{}{
		"token_refresh_needed": tokenRefreshNeeded,
		"user_name":            identity.Username,
	}, "is token refresh needed ?")

	// if tokenRefreshNeeded = true, then we can deduce without GET-ing keycloak profile
	// that we need to (1) update keycloak user profile (2) refresh token.

	profileUpdateNeeded := tokenRefreshNeeded
	if !tokenRefreshNeeded {
		profileEqual, err := keycloak.equalsKeycloakUserProfileAttributes(ctx, keycloakToken.AccessToken, *identity, accountAPIEndpoint)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":       err,
				"user_name": identity.Username,
			}, "keycloak profile comparison failed")
			return nil, err
		}
		profileUpdateNeeded = !profileEqual
		log.Info(ctx, map[string]interface{}{
			"profile_updated_needed": profileUpdateNeeded,
			"user_name":              identity.Username,
		}, "is profile update needed ?")
	}

	profileUpdatePayload := keycloakUserRequestFromIdentity(*identity)
	if profileUpdateNeeded {
		err = keycloak.updateUserInKeycloak(ctx, request, profileUpdatePayload, config, identity)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":       err,
				"user_name": identity.Username,
			}, "keycloak profile update failed")
			return nil, err
		}
	}

	if tokenRefreshNeeded {
		endpoint, err := config.GetKeycloakEndpointToken(request)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":       err,
				"user_name": identity.Username,
			}, "error getting endpoint")
			return nil, err
		}

		tokenSet, err := keycloak.keycloakTokenService.RefreshToken(ctx, endpoint, config.GetKeycloakClientID(), config.GetKeycloakSecret(), keycloakToken.AccessToken)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":               err,
				"keycloak_endpoint": endpoint,
				"user_name":         identity.Username,
			}, "refresh token failed")
			return nil, err
		}
		oauth2Token := &oauth2.Token{
			Expiry:       time.Unix(*tokenSet.ExpiresIn, 0),
			TokenType:    *tokenSet.TokenType,
			AccessToken:  *tokenSet.AccessToken,
			RefreshToken: *tokenSet.RefreshToken,
		}
		oauth2Token = oauth2Token.WithExtra(map[string]interface{}{
			"expires_in":         *tokenSet.ExpiresIn,
			"refresh_expires_in": *tokenSet.RefreshExpiresIn,
			"not_before_policy":  *tokenSet.NotBeforePolicy,
		})
		return oauth2Token, nil
	}

	return keycloakToken, err
}

// AuthCodeCallback takes care of authorization callback.
// When authorization_code is requested with /api/authorize, keycloak would return authorization_code at /api/authorize/callback,
// which would pass on the code along with the state to client using this method
func (keycloak *KeycloakOAuthProvider) AuthCodeCallback(ctx *app.CallbackAuthorizeContext) (*string, error) {
	referrerURL, responseMode, err := keycloak.reclaimReferrerAndResponseMode(ctx, ctx.State, ctx.Code)
	if err != nil {
		return nil, err
	}
	var redirectTo string
	parameters := referrerURL.Query()
	parameters.Add("code", ctx.Code)
	parameters.Add("state", ctx.State)

	if responseMode != nil && *responseMode == "fragment" {
		referrerURL.Fragment = parameters.Encode()
	} else {
		referrerURL.RawQuery = parameters.Encode()
	}
	redirectTo = referrerURL.String()

	return &redirectTo, nil
}

// reclaimReferrer reclaims referrerURL and verifies the state
func (keycloak *KeycloakOAuthProvider) reclaimReferrerAndResponseMode(ctx context.Context, state string, code string) (*url.URL, *string, error) {
	knownReferrer, responseMode, err := keycloak.getReferrerAndResponseMode(ctx, state)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"state": state,
			"err":   err,
		}, "unknown state")
		return nil, nil, autherrors.NewUnauthorizedError("unknown state: " + err.Error())
	}
	referrerURL, err := url.Parse(knownReferrer)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"code":           code,
			"state":          state,
			"known_referrer": knownReferrer,
			"err":            err,
		}, "failed to parse referrer")
		return nil, nil, autherrors.NewInternalError(ctx, err)
	}

	log.Debug(ctx, map[string]interface{}{
		"code":           code,
		"state":          state,
		"known_referrer": knownReferrer,
		"response_mode":  responseMode,
	}, "referrer found")

	return referrerURL, responseMode, nil
}

func encodeToken(ctx context.Context, referrer *url.URL, outhToken *oauth2.Token, apiClient string) error {
	tokenJson, err := TokenToJson(ctx, outhToken)
	if err != nil {
		return err
	}
	parameters := referrer.Query()
	if apiClient != "" {
		parameters.Add(apiTokenParam, tokenJson)
	} else {
		parameters.Add(tokenJSONParam, tokenJson)
	}
	referrer.RawQuery = parameters.Encode()
	return nil
}

func (keycloak *KeycloakOAuthProvider) saveParams(ctx context.Context, redirect string, apiClient *string) (*string, error) {
	if apiClient != nil {
		// We need to save"api_client" params so we don't lose them when redirect to sso for auth and back to auth.
		linkURL, err := url.Parse(redirect)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"redirect": redirect,
				"err":      err,
			}, "unable to parse redirect")
			return nil, autherrors.NewBadParameterError("redirect", redirect).Expected("valid URL")
		}
		parameters := linkURL.Query()
		if apiClient != nil {
			parameters.Add(apiClientParam, *apiClient)
		}
		linkURL.RawQuery = parameters.Encode()
		s := linkURL.String()
		return &s, nil
	}
	return &redirect, nil
}

func (keycloak *KeycloakOAuthProvider) saveReferrer(ctx context.Context, state string, referrer string, responseMode *string, validReferrerURL string) error {
	err := oauth.SaveReferrer(ctx, keycloak.App, state, referrer, responseMode, validReferrerURL)
	if err != nil {
		return err
	}
	return nil
}

func (keycloak *KeycloakOAuthProvider) getReferrerAndResponseMode(ctx context.Context, state string) (string, *string, error) {
	return oauth.LoadReferrerAndResponseMode(ctx, keycloak.App, state)
}

// CreateOrUpdateIdentityInDB creates a user and a keycloak identity. If the user and identity already exist then update them.
// Returns the user, identity and true if a new user and identity have been created
func (keycloak *KeycloakOAuthProvider) CreateOrUpdateIdentityInDB(ctx context.Context, accessToken string, configuration Configuration) (*account.Identity, bool, error) {

	newIdentityCreated := false
	claims, err := keycloak.TokenManager.ParseToken(ctx, accessToken)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"token": accessToken,
			"err":   err,
		}, "unable to parse the token")
		return nil, false, errors.New("unable to parse the token " + err.Error())
	}

	if err := token.CheckClaims(claims); err != nil {
		log.Error(ctx, map[string]interface{}{
			"token": accessToken,
			"err":   err,
		}, "invalid keycloak token claims")
		return nil, false, errors.New("invalid keycloak token claims " + err.Error())
	}

	if !claims.Approved {
		return nil, false, autherrors.NewUnauthorizedError(fmt.Sprintf("user '%s' is not approved", claims.Username))
	}

	keycloakIdentityID, _ := uuid.FromString(claims.Subject)

	identity := &account.Identity{}
	// TODO : Check this only if UUID is not null
	// If identity already existed in WIT, then IDs should match !
	if identity.Username != "" && keycloakIdentityID.String() != identity.ID.String() {
		log.Error(ctx, map[string]interface{}{
			"keycloak_identity_id": keycloakIdentityID,
			"wit_identity_id":      identity.ID,
			"err":                  err,
		}, "keycloak identity id and existing identity id in wit service does not match")
		return nil, false, errors.New("Keycloak identity ID and existing identity ID in WIT does not match")
	}

	identities, err := keycloak.Identities.Query(account.IdentityFilterByID(keycloakIdentityID), account.IdentityWithUser())
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"keycloak_identity_id": keycloakIdentityID,
			"err": err,
		}, "unable to  query for an identity by ID")
		return nil, false, errors.New("Error during querying for an identity by ID " + err.Error())
	}

	if len(identities) == 0 {
		// No Identity found, create a new Identity and User

		// Now that user/identity objects have been initialized, update it
		// from the token claims info.

		_, err = fillUser(claims, identity)
		if identity.User.Cluster == "" {
			identity.User.Cluster = configuration.GetOpenShiftClientApiUrl()
		}
		if identity.User.FeatureLevel == "" {
			identity.User.FeatureLevel = account.DefaultFeatureLevel
		}
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"keycloak_identity_id": keycloakIdentityID,
				"err": err,
			}, "unable to create user/identity")
			return nil, false, errors.New("failed to update user/identity from claims" + err.Error())
		}

		err = transaction.Transactional(keycloak.App, func(tr transaction.TransactionalResources) error {
			user := &identity.User
			err := tr.Users().Create(ctx, user)
			if err != nil {
				return err
			}

			identity.ID = keycloakIdentityID
			identity.ProviderType = account.KeycloakIDP
			identity.UserID = account.NullUUID{UUID: user.ID, Valid: true}
			identity.User = *user
			err = tr.Identities().Create(ctx, identity)
			return err
		})
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"keycloak_identity_id": keycloakIdentityID,
				"username":             claims.Username,
				"err":                  err,
			}, "unable to create user/identity")
			return nil, false, errors.New("failed to create user/identity " + err.Error())
		}
		newIdentityCreated = true
	} else {
		identity = &identities[0]

		// we had done a
		// keycloak.Identities.Query(account.IdentityFilterByID(keycloakIdentityID), account.IdentityWithUser())
		// so, identity.user should have been populated.

		if identity.User.ID == uuid.Nil {
			log.Error(ctx, map[string]interface{}{
				"identity_id": keycloakIdentityID,
			}, "Found Keycloak identity is not linked to any User")
			return nil, false, errors.New("found Keycloak identity is not linked to any User")
		}
	}
	return identity, newIdentityCreated, err
}

func (keycloak *KeycloakOAuthProvider) updateWITUser(ctx context.Context, identity *account.Identity, witURL string, identityID string) error {
	updateUserPayload := &app.UpdateUsersPayload{
		Data: &app.UpdateUserData{
			Attributes: &app.UpdateIdentityDataAttributes{
				Bio:      &identity.User.Bio,
				Company:  &identity.User.Company,
				Email:    &identity.User.Email,
				FullName: &identity.User.FullName,
				ImageURL: &identity.User.ImageURL,
				URL:      &identity.User.URL,
				Username: &identity.Username,
			},
		},
	}
	return keycloak.App.WITService().UpdateUser(ctx, updateUserPayload, identityID)
}

func generateGravatarURL(email string) (string, error) {
	if email == "" {
		return "", nil
	}
	grURL, err := url.Parse("https://www.gravatar.com/avatar/")
	if err != nil {
		return "", errs.WithStack(err)
	}
	hash := md5.New()
	hash.Write([]byte(email))
	grURL.Path += fmt.Sprintf("%v", hex.EncodeToString(hash.Sum(nil))) + ".jpg"

	// We can use our own default image if there is no gravatar available for this email
	// defaultImage := "someDefaultImageURL.jpg"
	// parameters := url.Values{}
	// parameters.Add("d", fmt.Sprintf("%v", defaultImage))
	// grURL.RawQuery = parameters.Encode()

	urlStr := grURL.String()
	return urlStr, nil
}

// equalsKeycloakUserProfile returns whether
// profile updated is needed & whether token refresh is needed.

func (keycloak *KeycloakOAuthProvider) equalsTokenClaims(ctx context.Context, claims *token.TokenClaims, identity account.Identity) bool {
	computedFullName := name.GenerateFullName(&claims.GivenName, &claims.FamilyName)
	if identity.Username != claims.Username ||
		identity.User.FullName != computedFullName ||
		identity.User.Company != claims.Company ||
		identity.User.Email != claims.Email ||
		identity.User.EmailVerified != claims.EmailVerified {
		log.Error(ctx, map[string]interface{}{
			"user":     identity.User,
			"claims":   claims,
			"fullName": computedFullName,
		}, "claims and local db data don't match")
		return false
	}
	return true
}

// equalsKeycloakUserProfileAttributes verifies the response from keycloak's user profile
// and returns true if it matches with the user information managed locally by the auth service.
func (keycloak *KeycloakOAuthProvider) equalsKeycloakUserProfileAttributes(ctx context.Context, accessToken string, identity account.Identity, userAPIEndpoint string) (bool, error) {
	profileEqual := true

	retrievedUserProfile, err := keycloak.keycloakProfileService.Get(ctx, accessToken, userAPIEndpoint)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"username": identity.Username,
			"err":      err,
		}, "error getting user's info from keycloak")

		return false, err
	}

	computedFullName := name.GenerateFullName(retrievedUserProfile.FirstName, retrievedUserProfile.LastName)

	if (retrievedUserProfile.Username == nil || identity.Username != *retrievedUserProfile.Username) ||
		(retrievedUserProfile.Email == nil || identity.User.Email != *retrievedUserProfile.Email) ||
		identity.User.FullName != computedFullName ||
		retrievedUserProfile.Attributes == nil ||
		(retrievedUserProfile.EmailVerified == nil || identity.User.EmailVerified != *retrievedUserProfile.EmailVerified) {
		profileEqual = false
	}
	keycloakAttributes := retrievedUserProfile.Attributes
	if keycloakAttributes == nil ||
		!equalsKeycloakAttribute(*keycloakAttributes, CompanyAttributeName, identity.User.Company) ||
		!equalsKeycloakAttribute(*keycloakAttributes, BioAttributeName, identity.User.Bio) ||
		!equalsKeycloakAttribute(*keycloakAttributes, ImageURLAttributeName, identity.User.ImageURL) ||
		!equalsKeycloakAttribute(*keycloakAttributes, ClusterAttribute, identity.User.Cluster) {

		profileEqual = false
	}

	log.Info(ctx, map[string]interface{}{
		"profile_equal": profileEqual,
	}, "is keycloak profile in sync with auth db ?")

	return profileEqual, nil
}

func fillUser(claims *token.TokenClaims, identity *account.Identity) (bool, error) {
	isChanged := false
	if identity.User.FullName != claims.Name || identity.User.Email != claims.Email || identity.User.Company != claims.Company || identity.Username != claims.Username || identity.User.ImageURL == "" {
		isChanged = true
	} else {
		return isChanged, nil
	}
	identity.User.FullName = claims.Name
	identity.User.Email = claims.Email
	identity.User.Company = claims.Company
	identity.User.EmailVerified = claims.EmailVerified
	identity.Username = claims.Username
	if identity.User.ImageURL == "" {
		image, err := generateGravatarURL(claims.Email)
		if err != nil {
			log.Warn(nil, map[string]interface{}{
				"user_full_name": identity.User.FullName,
				"err":            err,
			}, "error when generating gravatar")
			// if there is an error, we will qualify the identity/user as unchanged.
			return false, errors.New("Error when generating gravatar " + err.Error())
		}
		identity.User.ImageURL = image
	}
	return isChanged, nil
}

// ContextIdentity returns the identity's ID found in given context
// Uses tokenManager.Locate to fetch the identity of currently logged in user
func ContextIdentity(ctx context.Context) (*uuid.UUID, error) {
	tm := tokencontext.ReadTokenManagerFromContext(ctx)
	if tm == nil {
		log.Error(ctx, map[string]interface{}{
			"token": tm,
		}, "missing token manager")

		return nil, errs.New("Missing token manager")
	}
	// As mentioned in token.go, we can now safely convert tm to a token.Manager
	manager := tm.(token.Manager)
	uuid, err := manager.Locate(ctx)
	if err != nil {
		// TODO : need a way to define user as Guest
		log.Error(ctx, map[string]interface{}{
			"uuid": uuid,
			"err":  err,
		}, "identity belongs to a Guest User")

		return nil, errs.WithStack(err)
	}
	return &uuid, nil
}

// ContextIdentityIfExists returns the identity's ID found in given context if the identity exists in the Auth DB
// If it doesn't exist then an Unauthorized error is returned
func ContextIdentityIfExists(ctx context.Context, app application.Application) (uuid.UUID, error) {
	identity, err := ContextIdentity(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	// Check if the identity exists
	err = transaction.Transactional(app, func(tr transaction.TransactionalResources) error {
		err := tr.Identities().CheckExists(ctx, identity.String())
		if err != nil {
			return autherrors.NewUnauthorizedError(err.Error())
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return *identity, nil
}

// LoadContextIdentityAndUser returns the identity found in given context if the identity exists in the Auth DB
// If no token present in the context then an Unauthorized error is returned
// If the identity represented by the token doesn't exist in the DB or not associated with any User then an Unauthorized error is returned
func LoadContextIdentityAndUser(ctx context.Context, app application.Application) (*account.Identity, error) {
	var identity *account.Identity
	identityID, err := ContextIdentity(ctx)
	if err != nil {
		return nil, autherrors.NewUnauthorizedError(err.Error())
	}
	// Check if the identity exists
	err = transaction.Transactional(app, func(tr transaction.TransactionalResources) error {
		identity, err = tr.Identities().LoadWithUser(ctx, *identityID)
		if err != nil {
			return autherrors.NewUnauthorizedError(err.Error())
		}
		return nil
	})
	return identity, err
}

// LoadContextIdentityIfNotDeprovisioned returns the same identity as LoadContextIdentityAndUser()
// if the user is not deprovisioned. Returns an Unauthorized error if the user is deprovisioned.
func LoadContextIdentityIfNotDeprovisioned(ctx context.Context, app application.Application) (*account.Identity, error) {
	identity, err := LoadContextIdentityAndUser(ctx, app)
	if err != nil {
		return nil, err
	}
	if identity.User.Deprovisioned {
		return nil, autherrors.NewUnauthorizedError("user deprovisioined")
	}
	return identity, err
}

// InjectTokenManager is a middleware responsible for setting up tokenManager in the context for every request.
func InjectTokenManager(tokenManager token.Manager) goa.Middleware {
	return func(h goa.Handler) goa.Handler {
		return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
			ctxWithTM := tokencontext.ContextWithTokenManager(ctx, tokenManager)
			return h(ctxWithTM, rw, req)
		}
	}
}

// TokenToJson marshals an oauth2 token to a json string
func TokenToJson(ctx context.Context, outhToken *oauth2.Token) (string, error) {
	str := outhToken.Extra("expires_in")
	var expiresIn interface{}
	var refreshExpiresIn interface{}
	var err error
	expiresIn, err = token.NumberToInt(str)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"expires_in": str,
			"err":        err,
		}, "unable to parse expires_in claim")
		return "", errs.WithStack(err)
	}
	str = outhToken.Extra("refresh_expires_in")
	refreshExpiresIn, err = token.NumberToInt(str)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"refresh_expires_in": str,
			"err":                err,
		}, "unable to parse expires_in claim")
		return "", errs.WithStack(err)
	}
	tokenData := &app.TokenData{
		AccessToken:      &outhToken.AccessToken,
		RefreshToken:     &outhToken.RefreshToken,
		TokenType:        &outhToken.TokenType,
		ExpiresIn:        &expiresIn,
		RefreshExpiresIn: &refreshExpiresIn,
	}
	b, err := json.Marshal(tokenData)
	if err != nil {
		return "", errs.WithStack(err)
	}

	return string(b), nil
}
