// 各 region 的 openapi 端点。**cn 与国际版是两套域名**：
//   cn     → openapi.qoder.com.cn（网页 qoder.com.cn）
//   global → openapi.qoder.sh    （网页 qoder.sh）
//
// 用户 m00305：「qoder 国际版不能签到吗？」
// —— 之前这里**只有 cn**，国际版账号会直接抛 `qoder_checkin_region_unsupported`。
//
// 实证国际版上游是支持的：国际版 CLI（@qoder-ai/qodercli）里签到相关代码
// 与 CN 版**逐项一致** —— `/sash/api/v1/me/campaigns`(1)、`getMachineToken`(3)、
// `claimActivity`(8)、`requiresCanClaim`(13)、`claimDisplay`(7) 计数完全相同，
// 端点表里 `openapi` 就是 `openapi.qoder.sh`。路径也同一条。
// 所以只是我们漏配了域名，不是上游不支持。
const endpoints = {
  cn: { base: "https://openapi.qoder.com.cn", origin: "https://qoder.com.cn" },
  global: { base: "https://openapi.qoder.sh", origin: "https://qoder.sh" },
};
const campaignsPath = "/sash/api/v1/me/campaigns";
const maxResponseBytes = 65536;

function object(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function campaignId(value) {
  return typeof value === "string" && value !== "" ? value : "";
}

async function readJSON(response) {
  if (!response.body) throw new Error("qoder_checkin_empty_response");
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maxResponseBytes) throw new Error("qoder_checkin_response_too_large");
      chunks.push(value);
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
  let payload;
  try {
    payload = JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw new Error("qoder_checkin_invalid_json");
  }
  if (!object(payload)) throw new Error("qoder_checkin_invalid_response");
  return payload;
}

function campaignsFrom(payload) {
  if ("campaigns" in payload) {
    if (!Array.isArray(payload.campaigns)) throw new Error("qoder_checkin_invalid_response");
    return payload.campaigns.filter(object);
  }
  if (object(payload.data) && Array.isArray(payload.data.campaigns)) return payload.data.campaigns.filter(object);
  if (Array.isArray(payload.data)) return payload.data.filter(object);
  return [];
}

function unwrapClaim(payload) {
  return object(payload.data) ? payload.data : payload;
}

function creditCampaigns(items) {
  return items.filter((item) => item.actionType === "CLAIM_BENEFIT" && campaignId(item.campaignId));
}

function claimStatus(items, id) {
  const item = items.find((campaign) => campaign.campaignId === id);
  return item?.claimStatus;
}

function rewardFrom(campaign) {
  const benefit = object(campaign.benefit) ? campaign.benefit : null;
  if (!benefit || benefit.kind !== "CREDITS") return;
  if (typeof benefit.amount !== "number" || !Number.isFinite(benefit.amount) || benefit.amount < 0) return;
  return benefit.amount;
}

// 从 Qoder 桌面 App 自带的**原生 UMID 二进制**取机器身份。
//
// 为什么需要（用户 m00312：「国际版签到积分没变」，实测根因）：
// 同一个账号、同样的请求头，只换 `Cosy-MachineToken` 的来源，
// 上游 `/sash/api/v1/me/campaigns` 返回的活动列表**完全不同**：
//
//   用 CLI 的 `_T().getMachineToken()` → 只有 VIEW_DETAILS，没有签到活动
//   用 App 原生二进制的 machineToken  → 出现 act-20260930-314
//                                       CLAIM_BENEFIT / CLAIMABLE（+100 Credits）
//
// 两个令牌都是 88 位、都以 `P1gA` 开头，但内容不同：CLI 的 `doInitialize()`
// 会 `extractBinary()` 出它**自己那份** umid 二进制，与 App 内的
// `Resources/umid/runtime-info` 不是同一个。上游按该令牌判定活动资格。
//
// 二进制输出（实测）：
//   {"machineToken":"P1gA...","machineType":"6448...","machineCode":"1a67...","vmInfo":{...}}
// machineCode / machineType 也要作为 `Cosy-MachineCode` / `Cosy-MachineType`
// 发给上游 —— App 的请求头构造函数 `YBr()` 就带这两个。
//
// 读不到就返回 null，调用方回落到 CLI 令牌：不能因为用户没装桌面 App
// 就让签到彻底不可用。
let nativeIdentityCache; // undefined=未取过 / null=取不到 / {token,code,type,at}
// 缓存有效期取 1 小时，与 CLI 的刷新周期一致：
// CLI 的 `scheduleNextRefresh()` 用 `y4a=36e5`（=3600s）加 ±5min 抖动重取令牌。
// 令牌虽然实测在多次调用间稳定，但**不能永久缓存** —— 上游可能做时效/防重放校验，
// 缓存过久会导致签到在跑了一段时间后莫名失败。1 小时足够省掉重复起进程的开销
// （二进制启动约 1s），又与 CLI 行为对齐。
const nativeIdentityTTLMs = 3600 * 1000;

async function nativeMachineIdentity(region) {
  const now = Date.now();
  if (nativeIdentityCache && nativeIdentityCache.at && now - nativeIdentityCache.at < nativeIdentityTTLMs) {
    return nativeIdentityCache;
  }
  if (nativeIdentityCache === null) return null;
  try {
    const { execFileSync } = await import("node:child_process");
    const { existsSync } = await import("node:fs");
    // 国际版与国内版是两个 App，优先按 region 选，另一个作兜底。
    const candidates = region === "cn"
      ? ["/Applications/Qoder CN.app/Contents/Resources/umid/runtime-info",
         "/Applications/Qoder.app/Contents/Resources/umid/runtime-info"]
      : ["/Applications/Qoder.app/Contents/Resources/umid/runtime-info",
         "/Applications/Qoder CN.app/Contents/Resources/umid/runtime-info"];
    for (const bin of candidates) {
      if (!existsSync(bin)) continue;
      try {
        const out = execFileSync(bin, [], { timeout: 15000, encoding: "utf8" });
        const parsed = JSON.parse(String(out).trim());
        const token = typeof parsed.machineToken === "string" ? parsed.machineToken.trim() : "";
        if (!token) continue;
        nativeIdentityCache = {
          token,
          code: typeof parsed.machineCode === "string" ? parsed.machineCode.trim() : "",
          type: typeof parsed.machineType === "string" ? parsed.machineType.trim() : "",
          at: Date.now(),
        };
        return nativeIdentityCache;
      } catch {
        // 换下一个候选
      }
    }
  } catch {
    // 忽略：回落到 CLI 令牌
  }
  nativeIdentityCache = null;
  return null;
}

async function machineHeaders(auth, region = "cn") {
  let machineId = auth.machineId;
  if (typeof auth.getMachineId === "function") {
    try {
      machineId = await auth.getMachineId();
    } catch {
      machineId = "";
    }
  }
  if (typeof machineId !== "string" || !machineId) return {};

  // **`Cosy-MachineToken` 必须是 UMID 机器令牌，不是 machineId**（用户 m00303）。
  //
  // 原来这里两行都填 machineId，是错的。CLI 自己的实现（qoderclicn.js 的
  // `buildHeaders`）写得很明确：
  //     let a = _T(); await a.initialize(uid); let l = a.getMachineToken();
  //     if (!l) throw new Error("Dynamic command request requires a UMID machine token");
  //     uWo(o, "Cosy-MachineToken", l)
  // 拿不到 UMID 令牌时 CLI 直接抛错 —— 说明上游会校验它。
  //
  // 我们原来拿 machineId 顶替，上游不认，于是签到恒返回
  // 「签到活动未开放」（其实是鉴权不通过）。
  //
  // 令牌由 `compat.mjs` 新注入的 `__qoderWorkerGetMachineToken()` 提供
  // （挂在 CLI 的 `_T()` 单例工厂上，内部会先 await initialize()）。
  let machineToken = "";
  try {
    if (typeof globalThis.__qoderWorkerGetMachineToken === "function") {
      machineToken = String((await globalThis.__qoderWorkerGetMachineToken()) || "");
    }
  } catch {
    machineToken = "";
  }

  // **优先用桌面 App 原生二进制的令牌**（用户 m00312 的根因修复）。
  //
  // 只换令牌来源、其余请求头完全一致时，上游返回的活动列表不同：
  //   CLI 令牌    → 只有 VIEW_DETAILS（看不到签到活动，签到恒 skipped）
  //   原生令牌    → 含 CLAIM_BENEFIT / CLAIMABLE（能真正签到 +100）
  // 所以能用原生令牌就用，取不到才回落到 CLI 令牌。
  const native = await nativeMachineIdentity(region);
  const effectiveToken = native?.token || machineToken;

  return {
    "Cosy-MachineId": machineId,
    // 拿不到 UMID 令牌时**不填**（而不是拿 machineId 冒充）：
    // 冒充只会让上游返回语义含糊的"活动未开放"，掩盖真实的鉴权失败。
    ...(effectiveToken ? { "Cosy-MachineToken": effectiveToken } : {}),
    // machineCode / machineType 与令牌同源，App 也会带这两个头。
    ...(native?.code ? { "Cosy-MachineCode": native.code } : {}),
    ...(native?.type ? { "Cosy-MachineType": native.type } : {}),
  };
}

export function createQoderCheckin({ region, getAuthManager, fetchImpl = (...args) => globalThis.fetch(...args) }) {
  let pending;

  async function execute() {
    const endpoint = endpoints[region];
    if (!endpoint) throw new Error("qoder_checkin_region_unsupported");
    const auth = getAuthManager();
    if (!auth?.isAuthenticated?.()) throw new Error("qoder_checkin_not_authenticated");
    if (typeof auth.getUserInfo !== "function" || typeof auth.refreshTokenIfNeeded !== "function") {
      throw new Error("qoder_checkin_auth_api_incompatible");
    }
    try {
      await auth.refreshTokenIfNeeded(undefined, "worker_checkin");
    } catch {
      throw new Error("qoder_checkin_auth_refresh_failed");
    }
    const machineHeadersValue = await machineHeaders(auth, region);

    async function request(path, method, refreshed = false) {
      const user = auth.getUserInfo();
      const token = user?.security_oauth_token ?? user?.access_token;
      if (typeof token !== "string" || !token) throw new Error("qoder_checkin_token_unavailable");
      let response;
      try {
        response = await fetchImpl(`${endpoint.base}${path}`, {
          method,
          headers: {
            Authorization: `Bearer ${token}`,
            Accept: "application/json",
            "Content-Type": "application/json",
            "User-Agent": "Qoder",
            "Cosy-ClientType": "10",
            "Cosy-Version": "0.3.4",
            ...machineHeadersValue,
            Origin: endpoint.origin,
            Referer: `${endpoint.base}/growth-page/activity-iframe`,
          },
          redirect: "manual",
          signal: AbortSignal.timeout(15000),
        });
      } catch {
        throw new Error("qoder_checkin_request_failed");
      }
      if (response.status === 401 && !refreshed && typeof auth.forceRefreshToken === "function") {
        await response.body?.cancel().catch(() => {});
        try {
          await auth.forceRefreshToken(undefined, "worker_checkin_unauthorized");
        } catch {
          throw new Error("qoder_checkin_auth_refresh_failed");
        }
        return request(path, method, true);
      }
      if (!response.ok) {
        await response.body?.cancel().catch(() => {});
        throw new Error(`qoder_checkin_http_${response.status}`);
      }
      return readJSON(response);
    }

    async function list() {
      return campaignsFrom(await request(campaignsPath, "GET"));
    }

    let items;
    try {
      items = await list();
    } catch (error) {
      if (error instanceof Error && error.message === "qoder_checkin_http_404") {
        return { status: "skipped", message: "签到活动未开放" };
      }
      throw error;
    }

    const benefits = creditCampaigns(items);
    const summary = items.map((item) => `${item.actionType || "unknown"}:${item.claimStatus || "none"}`).join(",");
    if (!benefits.length) {
      console.error("[checkin] no credit campaigns", { count: items.length, summary });
      return { status: "skipped", message: "签到活动未开放" };
    }
    const claimable = benefits.filter((item) => item.claimStatus === "CLAIMABLE");
    if (!claimable.length) {
      if (benefits.some((item) => item.claimStatus === "CLAIMED")) return { status: "already", message: "今日已签到" };
      console.error("[checkin] credit campaigns not claimable", { count: items.length, summary });
      return { status: "skipped", message: "签到活动未开放" };
    }

    let confirmed = 0;
    let recovered = 0;
    let reward = 0;
    let hasReward = false;
    for (const campaign of claimable) {
      const path = `${campaignsPath}/${encodeURIComponent(campaign.campaignId)}/claim`;
      try {
        const payload = unwrapClaim(await request(path, "POST"));
        if (payload.status !== "CLAIMED") throw new Error("qoder_checkin_claim_unconfirmed");
        confirmed += 1;
      } catch (error) {
        const current = claimStatus(await list().catch(() => []), campaign.campaignId);
        if (current === "CLAIMED") recovered += 1;
        else throw error;
      }
      const amount = rewardFrom(campaign);
      if (amount !== undefined) {
        hasReward = true;
        reward += amount;
      }
    }
    if (!confirmed && !recovered) throw new Error("qoder_checkin_claim_unconfirmed");
    if (!confirmed) return { status: "already", message: "已签到（复查确认）" };
    return {
      status: "success",
      message: hasReward ? `签到成功 +${reward} 积分` : "签到成功",
      ...(hasReward ? { reward_credits: reward } : {}),
    };
  }
  const fn = function checkin() {
    if (!pending) pending = execute().finally(() => { pending = undefined; });
    return pending;
  };
  return fn;
}
