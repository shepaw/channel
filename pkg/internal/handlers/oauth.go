package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	SceneID    string `json:"scene_id"`
	QRCodeURL  string `json:"qrcode_url"`
	Ticket     string `json:"ticket"`
	ExpireSeconds int `json:"expire_seconds"`
}

func (h *OAuthHandler) WechatQRCode(c *gin.Context) {
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
	redirectURI := url.QueryEscape(fmt.Sprintf("%s/auth/wechat/callback", h.config.BaseURL))
	qrcodeURL := fmt.Sprintf(
		"https://open.weixin.qq.com/connect/qrconnect?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_login&state=%s",
		appID, redirectURI, sceneID,
	)

	c.JSON(http.StatusOK, WechatQRCodeResponse{
		SceneID:       sceneID,
		QRCodeURL:     qrcodeURL,
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

	// 创建或获取用户
	user, err := h.authSvc.CreateOrGetUser(
		"wechat",
		wechatUser.UnionID,
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

// ===== Helper functions =====

type WechatTokenResponse struct {
	AccessToken  string `json:"access_token"`
	OpenID       string `json:"openid"`
	UnionID      string `json:"unionid"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type WechatUserInfo struct {
	OpenID     string `json:"openid"`
	NickName   string `json:"nickname"`
	HeadImgURL string `json:"headimgurl"`
	UnionID    string `json:"unionid"`
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