import fs from "node:fs";
import path from "node:path";

export const PINNED_QODERCLI_VERSION = "1.1.32";

export const NEEDLES = {
  prepareInfer: "prepareInferRequest(A,e,t,i){",
  createWasm:
    "async createWasmContext(){let A=await Ki();this.machineId||(this.machineId=await this.getMachineId()),HlA(this.machineId,A,JSON.stringify(this.getUserInfoForAuth()))}",
  modelCatalog: "function kn(){return r9e||(r9e=new o9e),r9e}",
  quotaApi:
    "function zf(){return B_t||(B_t=new nw(_e())),B_t}function tAe(){c4e.clear(),nFA.clear()}",
  checkinAuth: "getUserInfo(){return this.cachedUserInfo}",
  // UMID 机器信息管理器的单例工厂。签到必须带 `Cosy-MachineToken`（UMID 令牌），
  // 而这个令牌只能从这里拿 —— 见下方注入。
  machineToken: "function _T(){return f3A||(f3A=new tfA),f3A}",
  skipMain:
    "async function QNu(){let{main:A}=await Promise.resolve().then(()=>(Uds(),Fds));await A()}",
};

export function inspectQodercliSource(source, { version } = {}) {
  const text = String(source || "");
  const alreadyPatched =
    text.includes("__QODER_WORKER_INJECTED__") &&
    text.includes("__QODER_WORKER_MODEL_CATALOG__") &&
    text.includes("__QODER_WORKER_QUOTA_API__");
  const prepareInferFound = alreadyPatched || text.includes(NEEDLES.prepareInfer);
  const createWasmFound = alreadyPatched || text.includes(NEEDLES.createWasm);
  const modelCatalogFound = alreadyPatched || text.includes(NEEDLES.modelCatalog);
  const quotaApiFound = alreadyPatched || text.includes(NEEDLES.quotaApi);
  const machineTokenFound = alreadyPatched || text.includes(NEEDLES.machineToken);
  const checkinAuthFound = text.includes(NEEDLES.checkinAuth);
  const skipMainFound = alreadyPatched || text.includes(NEEDLES.skipMain);
  const ok = checkinAuthFound && (alreadyPatched || (prepareInferFound && createWasmFound && modelCatalogFound && quotaApiFound && machineTokenFound));
  const found = version ? `, found ${version}` : "";
  return {
    ok,
    alreadyPatched,
    prepareInferFound,
    createWasmFound,
    modelCatalogFound,
    quotaApiFound,
    checkinAuthFound,
    skipMainFound,
    prepareInferPatched: alreadyPatched,
    createWasmPatched: alreadyPatched,
    modelCatalogPatched: alreadyPatched,
    quotaApiPatched: alreadyPatched,
    skipMainPatched: alreadyPatched && text.includes("__QODER_WORKER_SKIP_MAIN__"),
    version: version || null,
    pinnedVersion: PINNED_QODERCLI_VERSION,
    message: ok
      ? `qodercli hooks compatible${version ? ` (${version})` : ""}`
      : `incompatible qodercli source: missing WASM/catalog/quota/auth needles (pinned ${PINNED_QODERCLI_VERSION}${found}). Pin @qoder-ai/qodercli@${PINNED_QODERCLI_VERSION} or @qodercn-ai/qoderclicn@${PINNED_QODERCLI_VERSION}, or update worker/src/compat.mjs.`,
  };
}

export function patchQodercliSource(source, { version } = {}) {
  const text = String(source || "");
  if (
    text.includes("__QODER_WORKER_INJECTED__") &&
    text.includes("__QODER_WORKER_MODEL_CATALOG__") &&
    text.includes("__QODER_WORKER_QUOTA_API__") && text.includes(NEEDLES.checkinAuth)
  ) {
    return text;
  }
  const report = inspectQodercliSource(text, { version });
  if (!report.ok) {
    throw new Error(report.message);
  }
  let next = text
    .replace(
      NEEDLES.prepareInfer,
      "prepareInferRequest(A,e,t,i){ /* __QODER_WORKER_INJECTED__ */ try{ if(typeof globalThis.__qoderWorkerOnPrepareInfer==='function'){ globalThis.__qoderWorkerOnPrepareInfer(this,A,e,t,i); } }catch(_e){} ",
    )
    .replace(
      NEEDLES.createWasm,
      "async createWasmContext(){ /* __QODER_WORKER_INJECTED__ */ try{ globalThis.__qoderAuthManager=this; }catch(_e){} let A=await Ki();this.machineId||(this.machineId=await this.getMachineId()); const __ctx=HlA(this.machineId,A,JSON.stringify(this.getUserInfoForAuth())); try{ if(typeof globalThis.__qoderWorkerAdoptContext==='function'){ globalThis.__qoderWorkerAdoptContext(__ctx,this); } }catch(_e2){} }",
    )
    .replace(
      NEEDLES.modelCatalog,
      "function kn(){return r9e||(r9e=new o9e),r9e} /* __QODER_WORKER_MODEL_CATALOG__ */ try{globalThis.__qoderWorkerGetModelCatalog=()=>{ph();return kn()}}catch(_e){}",
    )
    .replace(
      NEEDLES.machineToken,
      // 在 UMID 单例工厂上挂钩子：worker 通过它拿 `Cosy-MachineToken`。
      //
      // 为什么需要：CLI 的签到（campaign rewards claim）在 buildHeaders 里硬性要求
      // `_T().getMachineToken()`，拿不到就抛
      // 「Dynamic command request requires a UMID machine token」。
      // 而 worker 原来只能拿到 machineId、把它当 token 用 —— 上游不认，
      // 于是签到恒返回「签到活动未开放」（用户 m00303 实测到的现象）。
      //
      // 暴露的是**异步**取令牌：getMachineToken() 只在 cache 有效时返回，
      // 所以要先 await initialize()，这与 CLI 自己的 buildHeaders 完全一致。
      // 与 quotaApi 同一套路：**紧跟原函数**追加一段立即执行的 try/catch，
      // 在模块初始化时就注册钩子（而不是等 `_T()` 被调用 —— 实测 boot 流程
      // 并不会调它，挂在函数体内会导致钩子永远注册不上）。
      "function _T(){return f3A||(f3A=new tfA),f3A} /* __QODER_WORKER_MACHINE_TOKEN__ */ try{globalThis.__qoderWorkerGetMachineToken=async()=>{const __m=_T();try{const __u=(typeof _e==='function'?_e().getUserInfo?.():null)?.uid;await __m.initialize(__u)}catch(_e1){}return __m.getMachineToken?.()||''}}catch(_e2){}",
    )
    .replace(
      NEEDLES.quotaApi,
      "function zf(){return B_t||(B_t=new nw(_e())),B_t}function tAe(){c4e.clear(),nFA.clear()} /* __QODER_WORKER_QUOTA_API__ */ try{globalThis.__qoderWorkerGetQuotaApi=()=>{o5();return zf()}}catch(_e){}",
    );
  if (next.includes(NEEDLES.skipMain)) {
    next = next.replace(
      NEEDLES.skipMain,
      "async function QNu(){ /* __QODER_WORKER_INJECTED__ __QODER_WORKER_SKIP_MAIN__ */ if(typeof globalThis.__qoderWorkerBoot==='function'){ const {getQoderAuthManager}=await Promise.resolve().then(()=>(Ul(),D3A)); const {initializeQoderRuntime}=await Promise.resolve().then(()=>(eG(),FeA)); return globalThis.__qoderWorkerBoot({getQoderAuthManager,initializeQoderRuntime}); } let{main:A}=await Promise.resolve().then(()=>(Uds(),Fds));await A()}",
    );
  }
  return next;
}

export function readQodercliVersion(jsPath) {
  const candidates = [
    path.resolve(path.dirname(jsPath), "../../package.json"),
    path.resolve(path.dirname(jsPath), "../package.json"),
  ];
  for (const pkgPath of candidates) {
    try {
      const raw = fs.readFileSync(pkgPath, "utf8");
      const parsed = JSON.parse(raw);
      if (parsed?.name === "@qoder-ai/qodercli" || parsed?.name === "@qodercn-ai/qoderclicn" || parsed?.version) {
        return String(parsed.version || "");
      }
    } catch {}
  }
  return null;
}
