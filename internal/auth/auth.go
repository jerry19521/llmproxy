// Package auth 提供管理员 JWT 认证、密码校验、代理 Key 的 IP 白名单匹配与来源 IP 提取。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ===== JWT（HS256 自实现）=====

var (
	ErrTokenInvalid = errors.New("token 无效")
	ErrTokenExpired = errors.New("token 已过期")
)

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Sub      string `json:"sub"` // 用户名
	Iat      int64  `json:"iat"` // 签发时间
	Exp      int64  `json:"exp"` // 过期时间
	Password string `json:"pwd"` // 密码哈希摘要（密码修改后旧 token 失效）
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signHS256(data []byte, secret string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(data)
	return h.Sum(nil)
}

// IssueToken 签发 JWT。
func IssueToken(secret, username, passwordHash string, expireHours int) (token string, expiresAt time.Time) {
	now := time.Now()
	expiresAt = now.Add(time.Duration(expireHours) * time.Hour)
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(jwtClaims{
		Sub:      username,
		Iat:      now.Unix(),
		Exp:      expiresAt.Unix(),
		Password: shortHash(secret, passwordHash),
	})
	payload := b64url(claims)
	signing := header + "." + payload
	sig := b64url(signHS256([]byte(signing), secret))
	return signing + "." + sig, expiresAt
}

// VerifyToken 校验 JWT 并返回用户名；passwordHash 用于使修改密码后的旧 token 失效。
// 显式校验 header 的 alg 字段必须为 HS256：防算法混淆（alg:none / 未来扩展多算法时的降级攻击）。
func VerifyToken(token, secret, passwordHash string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrTokenInvalid
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrTokenInvalid
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil || hdr.Alg != "HS256" {
		return "", ErrTokenInvalid
	}
	signing := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", ErrTokenInvalid
	}
	if !hmac.Equal(sig, signHS256([]byte(signing), secret)) {
		return "", ErrTokenInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrTokenInvalid
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ErrTokenInvalid
	}
	if claims.Exp < time.Now().Unix() {
		return "", ErrTokenExpired
	}
	// v1.0.0（S10）：改用常量时间比较——旧实现的 != 短路比较与同文件
	// ConstantTimeEquals 的防护基调不一致，理论上可计时探测摘要字节。
	if !ConstantTimeEquals(claims.Password, shortHash(secret, passwordHash)) {
		return "", ErrTokenInvalid
	}
	return claims.Sub, nil
}

// shortHash 生成密码哈希的绑定摘要（密码修改后旧 token 失效的判定依据）。
// v1.0.0（S15）：改用 HMAC-SHA256(secret) 参与派生——旧实现为裸 SHA-256，
// token 泄漏后攻击者可用单次 SHA-256 对密码猜测做快速验证（每秒千万级），
// 把 bcrypt（每秒千级）降级为可快筛的等价物；secret 参与派生后快筛通道失效。
// 注意：摘要算法变更使升级前签发的旧 token 全部失效（重新登录一次即可）。
func shortHash(secret, s string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("llmproxy-pwd:" + s))
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

// ConstantTimeEquals 常量时间字符串相等比较（用于登录用户名比对，防计时探测）。
func ConstantTimeEquals(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// HashPassword 生成 bcrypt 哈希。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword 校验 bcrypt 密码。
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ===== IP 工具 =====

// ExtractClientIP 按反向代理层级提取来源 IP。
// level=0：直连 IP；level=1：XFF 首个 IP；level=2：XFF 倒数第二个 IP；依此类推。
// 层级超出 XFF 长度时回退到首个 IP，无 XFF 时回退直连 IP。
func ExtractClientIP(r *http.Request, level int) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if level <= 0 {
		return remote
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	var idx int
	if level == 1 {
		idx = 0 // 一层代理：取首个 IP
	} else {
		idx = len(parts) - level // 两层代理：取倒数第二个（len-2）；三层取倒数第三个（len-3），依此类推
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(parts) {
		idx = len(parts) - 1
	}
	if parts[idx] == "" {
		return remote
	}
	return parts[idx]
}

// normalizeIP 归一化 IP 字符串：IPv6 的大小写/压缩形式统一（如 FE80::1 → fe80::1），
// IPv4 统一为点分十进制；无法解析的字符串做 trim+小写处理并返回 valid=false。
// 归一化后精确匹配不受表示形式影响；前缀匹配要求 valid=true（见 MatchIPWhitelist）。
// 注意使用 net.IP.String()：net.ParseIP 对 IPv4 返回 4-in-6 十六字节形式，
// 经 netip.AddrFromSlice().String() 会输出带 ::ffff: 前缀的表示导致前缀匹配失效。
func normalizeIP(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip.String(), true
	}
	return strings.ToLower(s), false
}

// MatchIPWhitelist 判断 IP 是否匹配白名单。
// 规则：* 允许所有；x.y.* / x.* 点分前缀匹配；精确 IP 完全匹配。不支持 CIDR。
// 前缀规则要求以八位组分隔符结尾（TrimSuffix 后的 prefix 必须以 "." 结尾）：
// "10.1.*" → "10.1." 只匹配 10.1.x.x；非规范写法（如 "10*"、"10.1*"）视为无效规则
// 忽略（fail-safe），不会误放行 101.x.x.x / 10.11.x.x。
// IPv6 地址双方先归一化再比较（FE80::1 与 fe80::1 等价）。
func MatchIPWhitelist(ip string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return false
	}
	// v1.0.0（S11）：来源在 ip_trust_level≥1 时可来自不受控的 XFF——非 IP 字符串
	//（如 "10.1.bypass"）不得命中前缀规则，超出白名单「匹配 IP」的本意。
	ipNorm, ipValid := normalizeIP(ip)
	for _, rule := range whitelist {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if rule == "*" {
			return true
		}
		if strings.HasSuffix(rule, "*") {
			if !ipValid {
				continue // 非法 IP 串不做前缀匹配
			}
			prefix := strings.TrimSuffix(rule, "*")
			if prefix != "" && !strings.HasSuffix(prefix, ".") {
				continue // 非规范前缀规则：忽略，安全优先
			}
			if strings.HasPrefix(ipNorm, prefix) {
				return true
			}
			continue
		}
		if rNorm, _ := normalizeIP(rule); ipNorm == rNorm {
			return true
		}
	}
	return false
}
