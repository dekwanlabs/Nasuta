# 独立 OIDC 身份提供商服务 — 架构设计



**\-\-\-**



# **第一部分：通用 OIDC 服务能力**



## **一、定位**



新建一个独立的 OIDC 身份提供商（IdP）微服务，基于 **Spring Authorization Server**（新版），为所有需要 OIDC 协议对接的外部平台提供统一的身份认证能力。



**职责边界：** 只做身份认证和 token 颁发，不做业务逻辑。



**与现有服务的关系：**

- 新 OIDC 服务独立部署，通过 Feign 调现有用户服务获取用户信息

- `hsas-app-user-device` 新增接口，返回 loginUrl 给 App

**\-\-\-**



## **二、技术选型**



|项|选型|说明|
|---|---|---|
|框架|Spring Boot 2\.7\+ / 3\.x|取决于公司基础设施兼容性|
|授权服务器|Spring Authorization Server 1\.x|内置完整 OIDC 支持|
|JWT 签名|RSA RS256（框架内置）|无需手动引 nimbus\-jose\-jwt|
|注册中心|Nacos / Eureka（与现有一致）|—|
|配置中心|Apollo（与现有一致）|RSA 密钥、Client 配置|
|用户数据|通过 Feign 调现有用户服务|不直连用户数据库|
|缓存|Redis（与现有一致）|授权码、token 存储|



**\-\-\-**



## **三、数据流程图**



**数据流向说明：**

- 外部平台（Shopify 等）通过标准 OIDC 协议访问 `dreo-oidc-provider`

- OIDC 服务通过 Feign 调用户服务获取用户信息，不直连用户数据库

- `hsas-app-user-device` 负责生成 loginUrl（含 login\_hint），写 Redis 存一次性 key

- OIDC 服务读 Redis 消费 key，读 Apollo 加载 Client 配置和 RSA 密钥

**\-\-\-**



## **四、核心设计**



### **4\.1 需要自行实现的部分**



|模块|说明|
|---|---|
|`SilentAuthenticationProvider`|prompt=none 时：读 login\_hint → 解析用户 → 静默授权|
|`FormAuthenticationProvider`|非静默时：接收登录表单 → 调 UserFeign 校验密码 → 认证|
|`/login` 登录页|Thymeleaf 模板，邮箱\+密码表单，非静默场景使用|
|`LoginHintResolver` 策略接口|根据 client\_id 选择解析方式（邮箱 or Key）|
|`OidcTokenCustomizer`|给 id\_token 加 email/name 等自定义 claims|
|`UserFeign`|调现有用户服务查 email/userId/校验密码|
|`RegisteredClientRepository`|Client 配置加载（Apollo）|
|RSA 密钥加载|从 Apollo 读密钥对注入 JWKSource|



### **4\.2 框架自动提供的（零代码）**



|端点|说明|
|---|---|
|`/.well-known/openid-configuration`|OIDC 发现文档|
|`/oauth2/authorize`|授权端点|
|`/oauth2/token`|Token 端点|
|`/oauth2/jwks`|JWK Set 公钥端点|
|`/userinfo`|用户信息端点|
|`/oauth2/revoke`|Token 撤销|
|`/connect/logout`|RP\-Initiated Logout|



### **4\.3 认证模式（静默 / 非静默）**



OIDC 服务同时支持两种认证模式，通过请求参数自动区分，无需为不同平台写不同逻辑：



|模式|触发条件|行为|适用场景|
|---|---|---|---|

\| **静默认证** \| \`prompt=none\` 且带 \`login\_hint\` \| 不弹登录页，直接根据 login\_hint 解析用户身份完成授权 \| App 内嵌 WebView，用户已登录 App \|

\| **非静默认证** \| 不带 \`prompt=none\`（或 \`prompt=login\`） \| 跳转到登录页，用户手动输入账号密码 \| 浏览器直接访问店铺点登录 \|



**认证流程判断逻辑：**



```Plain Text
收到 /oauth2/authorize 请求
  ├── prompt=none 且有 login_hint
  │     → 走静默：LoginHintResolverFactory 解析用户 → 直接颁发授权码
  │     → 解析失败则返回 error=login_required（OIDC 规范要求）
  │
  └── 其他情况
        → 走正常流程：重定向到登录页 → 用户输入凭证 → 认证通过后颁发授权码
```



**两种模式对同一个 Client 都生效。** 比如 Shopify CA 站点：

- App 内走静默（`prompt=none` \+ `login_hint`）

- 用户在浏览器打开 Shopify 店铺点"登录" → Shopify 不带 `prompt=none` → 跳登录页

**\-\-\-**



#### **静默认证（prompt=none）**



不需要页面，纯后端逻辑：



1. 从请求参数取 `login_hint`

2. 通过 `LoginHintResolverFactory` 根据 client\_id 选策略解析用户

3. 解析成功 → 认证通过 → 框架自动颁发授权码，302 回 RP

4. 解析失败 → 返回 `error=login_required`（OIDC 规范要求，不能弹页面）

**\-\-\-**



#### **非静默认证（登录页）**



需要实现一个登录页面 \+ 后端认证逻辑：



**页面（HTML/Thymeleaf）：**

- 邮箱输入框 \+ 密码输入框 \+ 登录按钮

- 错误提示（密码错误、账号不存在等）

- 多语言支持（按接入平台的用户群决定）

- 样式与 Dreo 品牌统一（可后期优化）

**后端流程：**

```Plain Text
用户提交表单（email + password）
  → OIDC 服务调 UserFeign 校验账号密码
  → 验证通过 → Spring Security 标记认证成功
  → 框架自动继续 OIDC 授权流程（颁发 code，302 回 RP）
  → 验证失败 → 返回登录页并显示错误信息
```



**需要实现的代码：**



|组件|说明|
|---|---|
|`/login` 页面|Thymeleaf 模板，表单提交到 `/login` POST|
|`FormAuthenticationProvider`|接收 email\+password，调 UserFeign 验证|
|`UserFeign.verifyPassword(email, password)`|用户服务需提供密码校验接口|
|Spring Security 配置|`.formLogin().loginPage("/login")` 指定自定义登录页|



**和静默模式的关系：**

- 共用同一个 `/oauth2/authorize` 入口

- 框架判断用户未认证时才跳登录页

- 认证通过后，后续流程完全一致（颁发 code → token → id\_token）

**\-\-\-**



#### **整体认证架构**



```Plain Text
/oauth2/authorize
     │
     ├── 用户已认证（有 session）→ 直接颁发授权码
     │
     ├── prompt=none + login_hint → SilentAuthenticationProvider
     │     ├── 成功 → 颁发授权码
     │     └── 失败 → error=login_required
     │
     └── 用户未认证 → 302 到 /login
           → 用户提交表单 → FormAuthenticationProvider
           → 成功 → 建立 session → 回到 /oauth2/authorize → 颁发授权码
           → 失败 → 重新显示登录页
```



**\-\-\-**



### **4\.4 Client 注册（Apollo 配置驱动）**



每接入一个新平台，在 Apollo 配置中心新增一个 Client 配置项即可，**不需要数据库**，不需要改代码发版：



```YAML
oidc:
  clients:
    - clientId: shopify-ca
      clientSecret: '{bcrypt}$2a$10$...'
      scopes: openid,email,profile
      redirectUris: https://shopify提供的回调地址
      authorizationGrantTypes: authorization_code,refresh_token
      hintResolverType: KEY
      accessTokenTtl: 3600       # 1小时
      refreshTokenTtl: 2592000   # 30天

    - clientId: shopify-fr
      clientSecret: '{bcrypt}$2a$10$...'
      scopes: openid,email,profile
      redirectUris: https://shopify-fr提供的回调地址
      authorizationGrantTypes: authorization_code,refresh_token
      hintResolverType: KEY
      accessTokenTtl: 3600
      refreshTokenTtl: 2592000
```



**字段说明：**



|字段|说明|
|---|---|
|clientId|RP 后台生成的 Client ID|
|clientSecret|BCrypt 加密后的密钥|
|scopes|授权范围|
|redirectUris|RP 提供的回调地址，必须完全匹配|
|authorizationGrantTypes|授权方式|
|hintResolverType|策略类型：`KEY`（一次性 key）或 `EMAIL`（邮箱）|
|accessTokenTtl|access\_token 有效期（秒）|
|refreshTokenTtl|refresh\_token 有效期（秒）|



**实现方式：**

```Java
@Component
public class ApolloRegisteredClientRepository implements RegisteredClientRepository {
    @Value("${oidc.clients}")
    private List<ClientConfig> clients;

    @Override
    public RegisteredClient findByClientId(String clientId) {
        return clients.stream()
            .filter(c -> c.getClientId().equals(clientId))
            .map(this::toRegisteredClient)
            .findFirst()
            .orElse(null);
    }
}
```



Apollo 支持热更新，新增站点后配置推送即可生效，无需重启服务。



**\-\-\-**



## **五、服务间调用关系**



- `dreo-oidc-provider` 是纯 OIDC 服务，外部平台按标准协议对接

- `hsas-app-user-device` 新增接口返回 loginUrl 指向外部平台，外部平台再跳到 OIDC 服务

- 两个服务都通过 Feign 调用户服务获取用户信息

**\-\-\-**



## **六、login\_hint 安全方案（两种实现）**



### **背景**



OIDC 规范中 `login_hint` 是授权请求的可选参数，格式由 IdP 自行决定，可以是邮箱、手机号、或任意标识符。



当 RP（外部平台）发起静默授权时，会把 `login_hint` 传给 IdP。不同 RP 对该字段的格式要求不同：

- 有的 RP 要求必须是邮箱

- 有的 RP 只做透传，不校验格式

本服务提供两种内置解析策略，接入平台通过 `hint_resolver_type` 配置选择。



**\-\-\-**



### **方案 A：传邮箱（\`hint\_resolver\_type = EMAIL\`）**



**适用条件：** RP 要求 \`login\_hint\` 必须为邮箱格式。



**流程简述：**

```Plain Text
调用方 → 生成 loginUrl（login_hint=用户邮箱）→ RP → 302 到 IdP
IdP 收到 login_hint → EmailLoginHintResolver → userFeign.findByEmail(email) → 静默授权
```



**风险：** 邮箱在 URL 中明文传输，用户侧抓包可将 email 改为他人 → 以他人身份完成认证。依赖 HTTPS 传输安全，实际攻击难度较高但风险存在。



**\-\-\-**



### **方案 B：传一次性 Key（\`hint\_resolver\_type = KEY\`，推荐）**



**适用条件：** RP 不校验 \`login\_hint\` 格式，原样透传即可。



**流程简述：**

```Plain Text
调用方 → 生成 UUID key → 存 Redis（oidc:hint:{key} → {userId, email}，TTL 5分钟）
→ 生成 loginUrl（login_hint={key}）→ RP → 302 到 IdP
IdP 收到 login_hint → KeyLoginHintResolver → Redis 读取并立即删除 → 静默授权
```



**安全性：**

- key 是 UUID 随机值，不可预测

- 5分钟过期，用完即删

- 攻击者改了 key → Redis 查不到 → 认证失败

- 即使攻击者看到了原始 key，也只能用一次

**\-\-\-**



### **两方案对比**



|维度|方案 A（传邮箱）|方案 B（传 Key）|
|---|---|---|
|安全性|有篡改冒用风险|篡改无效，安全|
|Redis 依赖|OIDC 服务不需要额外 Redis 操作|调用方写 Redis，OIDC 服务读 Redis|
|复杂度|简单|多一次 Redis 读写|
|RP 兼容性|所有 RP 都支持|需确认 RP 不校验 login\_hint 格式|
|适用场景|RP 强制邮箱时的兜底方案|优先选择|



### **选择原则**



\- 新接入平台**默认用方案 B**（更安全）

- 只有确认 RP 会校验 login\_hint 必须为邮箱格式时，才退回方案 A

- 通过 `registered_client` 表的 `hint_resolver_type` 字段配置，无需改代码

**\-\-\-**



### **策略模式实现（配置驱动）**



每个接入平台在注册 Client 时选择自己的 \`login\_hint\` 解析方式（\`hint\_resolver\_type\` 字段），OIDC 服务根据配置自动选策略，**新增站点无需改代码**。



```Java
public interface LoginHintResolver {
    UserDTO resolve(String loginHint);
}

// 内置实现 A：邮箱解析
public class EmailLoginHintResolver implements LoginHintResolver {
    public UserDTO resolve(String loginHint) {
        return userFeign.findByEmail(loginHint);
    }
}

// 内置实现 B：一次性 Key 解析
public class KeyLoginHintResolver implements LoginHintResolver {
    public UserDTO resolve(String loginHint) {
        Map<String, Object> data = redis.get("oidc:hint:" + loginHint);
        redis.delete("oidc:hint:" + loginHint);  // 用完即删
        if (data == null) throw new OAuth2AuthenticationException("invalid key");
        return new UserDTO(data.get("userId"), data.get("email"));
    }
}

// 根据 client_id 读 Apollo 配置选策略
@Component
public class LoginHintResolverFactory {
    @Autowired
    private ApolloRegisteredClientRepository clientRepository;
    @Autowired
    private KeyLoginHintResolver keyResolver;
    @Autowired
    private EmailLoginHintResolver emailResolver;

    public LoginHintResolver getResolver(String clientId) {
        // 从 Apollo 配置读取 hint_resolver_type
        String type = clientRepository.getHintResolverType(clientId);
        if ("KEY".equals(type)) {
            return keyResolver;
        }
        return emailResolver;  // 默认走邮箱
    }
}
```



> 如果未来有平台需要自定义解析逻辑（不是邮箱也不是 key），只需新增一个 `LoginHintResolver` 实现类并注册到 Factory 即可。
> 
> 



**\-\-\-**



**\-\-\-**



## **七、新建平台接入步骤（通用）**



未来新增任何 OIDC 平台，只需：



1\. **Apollo 新增 Client 配置**：在 \`oidc\.clients\` 列表下新增一组配置项（client\_id、secret、redirect\_uri、scope、hint\_resolver\_type）

2\. **对方后台配置**：填入 \`https://oidc\.dreo\.com/\.well\-known/openid\-configuration\`、Client ID、Secret

3\. **完成**



无需改代码、无需发版、无需操作数据库。Apollo 推送后即时生效。



**\-\-\-**



## **八、部署信息**



|项|值|
|---|---|
|服务名|`dreo-oidc-provider`|
|端口|待定（建议 2023 或其他未占用端口）|
|域名|`oidc.dreo.com`（HTTPS，公网可访问）|
|依赖服务|用户服务（Feign）、Redis、Apollo、Nacos|



**\-\-\-**



## **九、风险与注意事项**



### **核心风险：无感知登录的安全边界**



\> **背景说明：** OIDC/OAuth2 协议本身的设计是"有感知登录"——用户主动在登录页认证自己的身份。我们的需求是把它改造成"无感知"（用户点订阅直接进结账页），这属于非标准用法。为实现无感知，我们用 \`login\_hint\` \+ \`prompt=none\` 跳过了用户主动认证环节，由此引入以下安全风险：



|风险|级别|说明|应对|
|---|---|---|---|

\| **login\_hint 身份篡改** \| 高（方案A）/ 低（方案B） \| 传邮箱时，用户抓包改 email 可冒充他人身份登录 Shopify 结账；传一次性 key 时，篡改后 Redis 查不到，认证失败 \| 优先方案 B（传 key）；方案 A 依赖 HTTPS 传输安全 \|

\| **无感知绕过了用户主动授权** \| 中 \| 标准 OIDC 要求用户主动确认授权，我们跳过了这步。如果 Dreo App token 泄露，攻击者可直接触发静默登录 \| Dreo App token 本身有时效和设备绑定机制 \|

\| **login\_hint 仅是"提示"非"认证"** \| 中 \| OIDC 规范中 login\_hint 设计初衷是预填登录信息，不是身份认证手段。我们把它当认证凭据用，超出了协议设计意图 \| 方案 B 的一次性 key 弥补了这个设计缺陷 \|

\| **Shopify 是否校验 login\_hint 格式** \| 待确认 \| 文档说"passed through"但未明确是否校验邮箱格式，直接决定能否用方案 B \| 上线前必须用测试店铺实测 \|



**\-\-\-**



### **技术风险**



|风险|说明|应对|
|---|---|---|
|Spring Boot 版本兼容|Spring Authorization Server 1\.x 要求 Boot 2\.7\+/3\.x，需确认公司基础框架版本|如果公司统一用 Boot 2\.3，需评估升级成本或独立打包|
|域名规划|新服务用 `oidc.dreo.com`，与旧 `oauth.dreo.com` 分开|Nginx 按域名分发|
|用户服务可用性|OIDC 服务依赖 UserFeign，用户服务挂了授权就挂了|超时\+熔断配置|
|login\_hint 静默授权|框架默认会弹同意页，需自定义 AuthenticationProvider 绕过|核心实现点|
|Redis 跨服务依赖（方案B）|user\-device 写、OIDC 服务读，需确保连同一个 Redis 集群|部署时确认 Redis 连接配置一致|



**\-\-\-**



## **十、Redis Key 汇总**



|Key|写入方|消费方|内容|TTL|
|---|---|---|---|---|
|`oidc:hint:{key}`|hsas\-app\-user\-device|dreo\-oidc\-provider|`{userId, email}`|5分钟|



> 仅方案 B 使用。方案 A 不涉及 Redis。
> 
> 授权码等 token 存储由 Spring Authorization Server 框架内部管理，无需手动操作。
> 
> 



**\-\-\-**



**\-\-\-**



# **第二部分：Shopify 接入**



## **十一、Shopify 多站点接入**



> 以下以 Shopify（新版 Customer Accounts）为例，演示如何利用上述通用 OIDC 服务完成接入。以加拿大站为首个接入站点，后续法国、西班牙等新站点按相同流程操作。
> 
> 



### **11\.1 背景**



Shopify 新版 Customer Accounts 不再支持 Multipass，要求商家接入符合 OIDC 协议的自有 IdP。Shopify 作为 RP（Relying Party），我们的 `dreo-oidc-provider` 作为 IdP。



业务需求：用户在 Dreo App 点订阅 → 无感知跳入 Shopify 结账页（不弹登录页）。



### **11\.2 Shopify 后台配置**



|配置项|值|说明|
|---|---|---|
|Identity Provider URL|`https://oidc.dreo.com/.well-known/openid-configuration`|Shopify 自动读取所有端点|
|Client ID|`shopify-ca`|填入后同步注册到我们的数据库|
|Client Secret|原始密钥|BCrypt 加密后存我们数据库|
|回调 URL|从 Shopify 后台复制|填入 `registered_client.redirect_uris`|



### **11\.3 Apollo 注册 Client（单站点示例）**



在 Apollo 配置中心的 `dreo-oidc-provider` 应用下新增：



```YAML
oidc:
  clients:
    - clientId: shopify-ca
      clientSecret: '{bcrypt}$2a$10$...'
      scopes: openid,email,profile
      redirectUris: https://shopify提供的回调地址
      authorizationGrantTypes: authorization_code,refresh_token
      hintResolverType: KEY      # Shopify CA 使用一次性 Key 方案
      accessTokenTtl: 3600       # 1小时
      refreshTokenTtl: 2592000   # 30天
```



配置推送后即时生效，无需重启服务。



### **11\.4 多站点配置**



每个 Shopify 店铺在各自后台独立配置 IdP，因此**每个站点对应一条独立的 Client 记录**：



|client\_id|店铺域名|redirect\_uri|hint\_resolver\_type|说明|
|---|---|---|---|---|
|`shopify-ca`|`ca.dreo.com`|Shopify CA 提供的回调地址|KEY|加拿大|
|`shopify-fr`|`fr.dreo.com`|Shopify FR 提供的回调地址|KEY|法国|
|`shopify-es`|`es.dreo.com`|Shopify ES 提供的回调地址|KEY|西班牙|



**为什么要分开：**

- Shopify 每个店铺独立生成 Client ID \+ Secret，不共用

- 每个店铺的 redirect\_uri 不同，OIDC 规范要求精确匹配

- 安全隔离：一个站点的凭据泄露不影响其他站点

**接入流程（每个新站点）：**

1. Shopify 新店铺后台 → 设置 Customer Accounts → 添加 Identity Provider

2. 填入 `https://oidc.dreo.com/.well-known/openid-configuration`

3. Shopify 生成 Client ID 和 Secret

4. Apollo 配置中心 `oidc.clients` 下新增一组配置（client\_id、secret、redirect\_uri）

5. 完成，无需改代码、无需发版、无需操作数据库

**多店铺共享同一个 IdP 的影响：**



|问题|说明|
|---|---|
|登录态是否共享|每个店铺独立跟 IdP 交互，Shopify 侧的 session 互相独立，不会出现"登了 CA 自动登了 FR"|
|登出时退哪个|Shopify 只会对当前店铺发起 `/connect/logout`，不影响其他店铺|
|Redis key 隔离|`oidc:hint:{key}` 是 UUID 随机生成，本身就不存在冲突，无需按站点前缀区分|



**\-\-\-**



### **11\.5 Shopify 场景下的 login\_hint 解析**



Shopify 使用 `hint_resolver_type = KEY`，OIDC 服务收到请求时自动选择 `KeyLoginHintResolver`：



```Plain Text
Shopify → /oauth2/authorize?login_hint={uuid-key}&prompt=none&client_id=shopify-ca
                                                                    ↓
                                            LoginHintResolverFactory.getResolver("shopify-ca")
                                                                    ↓
                                                        KeyLoginHintResolver.resolve(key)
                                                                    ↓
                                                    Redis: oidc:hint:{key} → {userId, email}
```



### **11\.6 hsas\-app\-user\-device 配置与接口**



#### **Apollo 配置（hsas\-app\-user\-device 侧）**



`hsas-app-user-device` 需要知道每个国家对应哪个 Shopify 店铺、用哪个域名。在 Apollo 配置：



```YAML
shopify:
  oidc:
    stores:
      CA:
        shopDomain: ca.dreo.com
        clientId: shopify-ca
      FR:
        shopDomain: fr.dreo.com
        clientId: shopify-fr
      ES:
        shopDomain: es.dreo.com
        clientId: shopify-es
```



#### **接口实现**



App 调用时传入国家码（`countryCode`），服务根据配置选择对应的 Shopify 店铺：



```Java
@GetMapping("/subscriptions/oauth/oidc-login-url")
public Result<OidcLoginUrlVO> getOidcLoginUrl(
        @UserId Long userId,
        @RequestParam String countryCode) {

    // 1. 根据国家码读配置，拿到对应店铺信息
    ShopifyStoreConfig store = shopifyOidcConfig.getStore(countryCode);
    if (store == null) {
        throw new BusinessException("不支持的国家: " + countryCode);
    }

    // 2. 查用户信息
    UserDTO user = userFeign.findById(userId);

    // 3. 生成一次性 key 存 Redis
    String key = UUID.randomUUID().toString();
    Map<String, Object> data = Map.of("userId", userId, "email", user.getEmail());
    redis.set("oidc:hint:" + key, data, 5, TimeUnit.MINUTES);

    // 4. 创建购物车拿 checkoutUrl（已有逻辑，按 countryCode 选店铺）
    String checkoutUrl = shopifyService.createCheckout(userId, countryCode);

    // 5. 拼 Shopify loginUrl（用配置里的域名）
    String loginUrl = String.format(
        "https://%s/customer_authentication/login?login_hint=%s&return_to=%s",
        store.getShopDomain(), key, URLEncoder.encode(checkoutUrl, "UTF-8")
    );

    return Result.success(new OidcLoginUrlVO(loginUrl));
}
```



#### **完整数据流**



```Plain Text
App 传入 countryCode=CA
  → hsas-app-user-device 读 Apollo 配置: CA → {shopDomain: ca.dreo.com, clientId: shopify-ca}
  → 生成 key 存 Redis
  → 拼 loginUrl: https://ca.dreo.com/customer_authentication/login?login_hint={key}&return_to=...
  → App 打开 loginUrl
  → Shopify CA 跳转到 OIDC 服务: /oauth2/authorize?client_id=shopify-ca&login_hint={key}
  → OIDC 服务读 Apollo: shopify-ca → hintResolverType=KEY
  → KeyLoginHintResolver 从 Redis 读取用户信息
  → 完成认证
```



**关键点：两边配置要对应一致**



|配置位置|配置项|必须匹配|
|---|---|---|
|hsas\-app\-user\-device Apollo|`shopify.oidc.stores.CA.clientId`|↕|
|dreo\-oidc\-provider Apollo|`oidc.clients[].clientId`|↕|
|Shopify 后台|Client ID|↕|



三处的 `clientId` 必须是同一个值，否则 OIDC 服务找不到对应 Client 配置会拒绝请求。



### **11\.7 完整时序图（Shopify \+ 方案 B）**



### **11\.8 Shopify 特有注意事项**



|项|说明|
|---|---|
|Shopify Plus|必须是 Plus 计划才支持自定义 IdP|
|login\_hint 格式|需实测确认 Shopify 是否校验格式（若校验则退回方案 A 传邮箱）|
|return\_to 限制|只能是 Shopify 同域下的相对路径，不能是外链|
|登出|Shopify 会调 `/connect/logout`，IdP 需清除对应 session|
|多店铺|每个店铺独立 client\_id，详见 11\.4 多站点配置|



### **11\.9 上线前验证清单**



* [ ] 测试店铺传非邮箱 login\_hint，确认 Shopify 是否透传

* [ ] 验证完整流程：App → loginUrl → Shopify → IdP → 结账页

* [ ] 验证 key 过期场景（超过 5 分钟）

* [ ] 验证 key 重放场景（同一个 key 用两次）

* [ ] 验证 id\_token 中的 claims 满足 Shopify 要求（sub, email, name）

* [ ] 验证登出流程

