package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type OAuthHandler struct {
	authSvc *services.AuthService
	redis   *services.RedisService
	config  *models.Config
}

func NewOAuthHandler(authSvc *services.AuthService, redis *services.RedisService, config *models.Config) *OAuthHandler {
	return &OAuthHandler{
		authSvc: authSvc,
		redis:   redis,
		config:  config,
	}
}

// ===== 微信OAuth =====

type WechatConfig struct {
	AppID     string
	AppSecret string
	BaseURL   string
}

type WechatQRCodeResponse struct {
	SceneID       string `json:"scene_id"`
	QRCodeURL     string `json:"qrcode_url"`
	AppID         string `json:"app_id,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	Ticket        string `json:"ticket"`
	ExpireSeconds int    `json:"expire_seconds"`
}

func (h *OAuthHandler) WechatQRCode(c *gin.Context) {
	// Dev 模式：返回假二维码数据，前端轮询时直接返回 token
	if os.Getenv("DEV_SKIP_AUTH") == "1" {
		sceneID := uuid.New().String()
		token, err := h.devGenerateToken()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "dev login failed"})
			return
		}
		// 直接写入已确认状态，前端首次轮询即可拿到 token
		h.redis.Set(fmt.Sprintf("wechat:scene:%s", sceneID), token, 5*time.Minute)
		c.JSON(http.StatusOK, WechatQRCodeResponse{
			SceneID:       sceneID,
			QRCodeURL:     "/static/dev-qrcode-placeholder.png",
			ExpireSeconds: 300,
		})
		return
	}

	// 在实际实现中，需要配置微信公众号或开放平台
	// 这里使用微信公众平台的网页授权二维码登录
	appID := h.config.WechatAppID
	if appID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "WeChat login not configured"})
		return
	}

	// 生成唯一场景ID用于轮询
	sceneID := uuid.New().String()

	// 缓存场景ID
	h.redis.Set(fmt.Sprintf("wechat:scene:%s", sceneID), "pending", 5*time.Minute)

	// 构建微信扫码登录URL（使用微信开放平台扫码登录方案）
	redirectURI := fmt.Sprintf("%s/auth/wechat/callback", h.config.BaseURL)
	qrcodeURL := fmt.Sprintf(
		"https://open.weixin.qq.com/connect/qrconnect?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_login&state=%s",
		appID, url.QueryEscape(redirectURI), sceneID,
	)

	c.JSON(http.StatusOK, WechatQRCodeResponse{
		SceneID:       sceneID,
		QRCodeURL:     qrcodeURL,
		AppID:         appID,
		RedirectURI:   redirectURI,
		ExpireSeconds: 300,
	})
}

func (h *OAuthHandler) WechatStatus(c *gin.Context) {
	sceneID := c.Query("scene_id")
	if sceneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scene_id required"})
		return
	}

	key := fmt.Sprintf("wechat:scene:%s", sceneID)
	status, err := h.redis.Get(key)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}

	if status == "pending" {
		c.JSON(http.StatusOK, gin.H{"status": "pending"})
		return
	}

	// 已确认，返回token
	c.JSON(http.StatusOK, gin.H{
		"status": "confirmed",
		"token":  status,
	})
	// 清理
	h.redis.Delete(key)
}

func (h *OAuthHandler) WechatCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state") // sceneID

	if code == "" {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=no_code")
		return
	}

	// 用code换取access_token
	wechatToken, err := exchangeWechatCode(h.config.WechatAppID, h.config.WechatAppSecret, code)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=wechat_exchange_failed")
		return
	}

	// 获取用户信息
	wechatUser, err := getWechatUserInfo(wechatToken.AccessToken, wechatToken.OpenID)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=wechat_userinfo_failed")
		return
	}

	providerID := wechatUser.UnionID
	if providerID == "" {
		providerID = wechatUser.OpenID
	}
	if providerID == "" {
		providerID = wechatToken.OpenID
	}

	// 创建或获取用户
	user, err := h.authSvc.CreateOrGetUser(
		"wechat",
		providerID,
		fmt.Sprintf("wx_%s@wechat.local", wechatUser.OpenID),
		wechatUser.NickName,
		wechatUser.HeadImgURL,
	)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=user_creation_failed")
		return
	}

	// 生成access token
	accessToken, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=token_generation_failed")
		return
	}

	// 如果有场景ID，更新场景状态（用于轮询）
	if state != "" {
		h.redis.Set(fmt.Sprintf("wechat:scene:%s", state), accessToken.Token, 5*time.Minute)
	}

	// 直接跳转到dashboard
	c.Redirect(http.StatusTemporaryRedirect, fmt.Sprintf("/oauth-callback?token=%s", accessToken.Token))
}

// ===== Google OAuth =====

func (h *OAuthHandler) GoogleInitiate(c *gin.Context) {
	// Dev 模式：跳过真实 OAuth，直接生成 token 并跳转
	if os.Getenv("DEV_SKIP_AUTH") == "1" {
		h.devLogin(c)
		return
	}

	clientID := h.config.GoogleClientID
	if clientID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Google login not configured"})
		return
	}

	state := uuid.New().String()
	h.redis.Set(fmt.Sprintf("google:state:%s", state), "1", 10*time.Minute)

	redirectURI := url.QueryEscape(fmt.Sprintf("%s/auth/google/callback", h.config.BaseURL))
	authURL := fmt.Sprintf(
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=email+profile&state=%s",
		clientID, redirectURI, state,
	)

	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

func (h *OAuthHandler) GoogleCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=no_code")
		return
	}

	// 验证state
	stateKey := fmt.Sprintf("google:state:%s", state)
	if _, err := h.redis.Get(stateKey); err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=invalid_state")
		return
	}
	h.redis.Delete(stateKey)

	// 换取token
	googleToken, err := exchangeGoogleCode(
		h.config.GoogleClientID,
		h.config.GoogleClientSecret,
		fmt.Sprintf("%s/auth/google/callback", h.config.BaseURL),
		code,
	)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=google_exchange_failed")
		return
	}

	// 获取用户信息
	googleUser, err := getGoogleUserInfo(googleToken.AccessToken)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=google_userinfo_failed")
		return
	}

	// 创建或获取用户
	user, err := h.authSvc.CreateOrGetUser(
		"google",
		googleUser.Sub,
		googleUser.Email,
		googleUser.Name,
		googleUser.Picture,
	)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=user_creation_failed")
		return
	}

	// 生成access token
	accessToken, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=token_generation_failed")
		return
	}

	c.Redirect(http.StatusTemporaryRedirect, fmt.Sprintf("/oauth-callback?token=%s", accessToken.Token))
}

// ===== GitHub OAuth =====

func (h *OAuthHandler) GitHubInitiate(c *gin.Context) {
	if os.Getenv("DEV_SKIP_AUTH") == "1" {
		h.devLogin(c)
		return
	}

	clientID := h.config.GitHubClientID
	if clientID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "GitHub login not configured"})
		return
	}

	state := uuid.New().String()
	h.redis.Set(fmt.Sprintf("github:state:%s", state), "1", 10*time.Minute)

	redirectURI := url.QueryEscape(fmt.Sprintf("%s/auth/github/callback", h.config.BaseURL))
	authURL := fmt.Sprintf(
		"https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&scope=read:user%%20user:email&state=%s",
		clientID, redirectURI, state,
	)

	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

func (h *OAuthHandler) GitHubCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=no_code")
		return
	}

	stateKey := fmt.Sprintf("github:state:%s", state)
	if _, err := h.redis.Get(stateKey); err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=invalid_state")
		return
	}
	h.redis.Delete(stateKey)

	redirectURI := fmt.Sprintf("%s/auth/github/callback", h.config.BaseURL)
	githubToken, err := exchangeGitHubCode(
		h.config.GitHubClientID,
		h.config.GitHubClientSecret,
		redirectURI,
		code,
	)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=github_exchange_failed")
		return
	}

	githubUser, err := getGitHubUserInfo(githubToken.AccessToken)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=github_userinfo_failed")
		return
	}

	email := githubUser.Email
	if email == "" {
		if primary, err := getGitHubPrimaryEmail(githubToken.AccessToken); err == nil {
			email = primary
		}
	}
	if email == "" {
		email = fmt.Sprintf("gh_%d@github.local", githubUser.ID)
	}

	name := githubUser.Name
	if name == "" {
		name = githubUser.Login
	}

	user, err := h.authSvc.CreateOrGetUser(
		"github",
		strconv.FormatInt(githubUser.ID, 10),
		email,
		name,
		githubUser.AvatarURL,
	)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=user_creation_failed")
		return
	}

	accessToken, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login?error=token_generation_failed")
		return
	}

	c.Redirect(http.StatusTemporaryRedirect, fmt.Sprintf("/oauth-callback?token=%s", accessToken.Token))
}

// ===== Helper functions =====

type WechatTokenResponse struct {
	AccessToken  string `json:"access_token"`
	OpenID       string `json:"openid"`
	UnionID      string `json:"unionid"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	ErrCode      int    `json:"errcode"`
	ErrMsg       string `json:"errmsg"`
}

type WechatUserInfo struct {
	OpenID     string `json:"openid"`
	NickName   string `json:"nickname"`
	HeadImgURL string `json:"headimgurl"`
	UnionID    string `json:"unionid"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

func exchangeWechatCode(appID, appSecret, code string) (*WechatTokenResponse, error) {
	apiURL := fmt.Sprintf(
		"https://api.weixin.qq.com/sns/oauth2/access_token?appid=%s&secret=%s&code=%s&grant_type=authorization_code",
		appID, appSecret, code,
	)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result WechatTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.ErrCode != 0 || result.AccessToken == "" {
		return nil, fmt.Errorf("wechat token: %d %s", result.ErrCode, result.ErrMsg)
	}

	return &result, nil
}

func getWechatUserInfo(accessToken, openID string) (*WechatUserInfo, error) {
	apiURL := fmt.Sprintf(
		"https://api.weixin.qq.com/sns/userinfo?access_token=%s&openid=%s&lang=zh_CN",
		accessToken, openID,
	)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result WechatUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.ErrCode != 0 || result.OpenID == "" {
		return nil, fmt.Errorf("wechat userinfo: %d %s", result.ErrCode, result.ErrMsg)
	}

	return &result, nil
}

type GoogleTokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

type GoogleUserInfo struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

func exchangeGoogleCode(clientID, clientSecret, redirectURI, code string) (*GoogleTokenResponse, error) {
	params := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code":          {code},
	}

	resp, err := http.Post(
		"https://oauth2.googleapis.com/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(params.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result GoogleTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

func getGoogleUserInfo(accessToken string) (*GoogleUserInfo, error) {
	req, err := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result GoogleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

type GitHubTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type GitHubUserInfo struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

type gitHubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func githubAPIRequest(method, apiURL, accessToken string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, apiURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "channel-service")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return http.DefaultClient.Do(req)
}

func exchangeGitHubCode(clientID, clientSecret, redirectURI, code string) (*GitHubTokenResponse, error) {
	params := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"code":          {code},
	}

	req, err := http.NewRequest(
		"POST",
		"https://github.com/login/oauth/access_token",
		strings.NewReader(params.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "channel-service")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result GitHubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Error != "" || result.AccessToken == "" {
		return nil, fmt.Errorf("github token: %s %s", result.Error, result.ErrorDescription)
	}

	return &result, nil
}

func getGitHubUserInfo(accessToken string) (*GitHubUserInfo, error) {
	resp, err := githubAPIRequest("GET", "https://api.github.com/user", accessToken, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github userinfo: status %d", resp.StatusCode)
	}

	var result GitHubUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.ID == 0 {
		return nil, fmt.Errorf("github userinfo: missing id")
	}

	return &result, nil
}

func getGitHubPrimaryEmail(accessToken string) (string, error) {
	resp, err := githubAPIRequest("GET", "https://api.github.com/user/emails", accessToken, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github emails: status %d", resp.StatusCode)
	}

	var emails []gitHubEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", err
	}

	var fallback string
	for _, e := range emails {
		if e.Email == "" {
			continue
		}
		if e.Primary && e.Verified {
			return e.Email, nil
		}
		if fallback == "" && e.Verified {
			fallback = e.Email
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	if len(emails) > 0 && emails[0].Email != "" {
		return emails[0].Email, nil
	}
	return "", fmt.Errorf("github emails: none")
}

// ===== Dev 模式辅助方法 =====

// devGenerateToken 在 DEV_SKIP_AUTH=1 时创建/获取 dev 用户并生成 token
func (h *OAuthHandler) devGenerateToken() (string, error) {
	devUserID := os.Getenv("DEV_USER_ID")
	if devUserID == "" {
		devUserID = devSkipAuthUserID
	}
	user, err := h.authSvc.CreateOrGetUser("dev", devUserID, "dev@local.test", "Dev User", "")
	if err != nil {
		return "", err
	}
	token, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		return "", err
	}
	return token.Token, nil
}

// devLogin 在 DEV_SKIP_AUTH=1 时直接生成 token 并重定向到 oauth-callback
func (h *OAuthHandler) devLogin(c *gin.Context) {
	token, err := h.devGenerateToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dev login failed: " + err.Error()})
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, fmt.Sprintf("/oauth-callback?token=%s", token))
}