/**
 * 二进制下载：把附件 URL 取成字节。
 *
 * 为什么不用 `fetch` 直接下：
 *   插件跑在宿主 iframe 里（`*.feishupkg.com` ⊂ `*.feishu.com`），
 *   官方给小组件/插件提供的网络通道是宿主注入的 `tt.request`（支持 `responseType: 'arraybuffer'`）。
 *   它比 `fetch` 更"正路"，而且在真实沙箱里更可能被允许（不存在跨域预检那一套）。
 *
 * 因此策略是：**能用 tt.request 就用它，否则退回 fetch**。
 * 两条路都必须带 `responseType=arraybuffer` 语义，拿到的都是字节。
 *
 * 并发：官方文档写明网络请求**最大并发 5**（超了会被拒或排队），所以这里固定 3 并发留余量。
 */
import type { PlannedFile } from './naming.js';

/** 宿主注入的 i18n/网络对象（非标准 DOM，按需探测）。 */
interface TtLike {
  request?: (opts: {
    url: string;
    method?: string;
    responseType?: 'text' | 'arraybuffer';
    header?: Record<string, string>;
    success?: (res: { statusCode: number; data: ArrayBuffer | string }) => void;
    fail?: (err: unknown) => void;
  }) => { abort?: () => void };
}

function tt(): TtLike | undefined {
  const g = globalThis as unknown as { tt?: TtLike };
  return g.tt?.request ? g.tt : undefined;
}

/** 是否走宿主网络通道（界面里显示出来，便于排查）。 */
export function transportName(): 'tt.request' | 'fetch' {
  return tt() ? 'tt.request' : 'fetch';
}

/** 下载一个 URL 为字节。 */
export async function fetchBytes(url: string): Promise<Uint8Array> {
  const injected = tt();
  if (injected?.request) {
    return new Promise<Uint8Array>((resolve, reject) => {
      injected.request!({
        url,
        method: 'GET',
        responseType: 'arraybuffer',
        success: (res) => {
          if (res.statusCode < 200 || res.statusCode >= 300) {
            reject(new Error(`HTTP ${res.statusCode}`));
            return;
          }
          if (typeof res.data === 'string') {
            reject(new Error('宿主返回了文本而非字节（responseType 未生效）'));
            return;
          }
          resolve(new Uint8Array(res.data));
        },
        fail: (err) => reject(new Error(`宿主网络失败：${String(err)}`)),
      });
    });
  }

  const resp = await fetch(url, { credentials: 'omit' });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  return new Uint8Array(await resp.arrayBuffer());
}

export interface DownloadedFile {
  plan: PlannedFile;
  data: Uint8Array;
}

export interface DownloadProgress {
  /** 已处理（成功+失败）的文件数。 */
  done: number;
  /** 计划下载的文件总数。 */
  total: number;
  /** 当前正在下载的文件（用于界面显示）。 */
  current?: string;
}

export interface DownloadFailure {
  plan: PlannedFile;
  error: string;
}

export interface DownloadOutcome {
  files: DownloadedFile[];
  failures: DownloadFailure[];
}

export interface DownloadParams {
  /** 待下载计划（core 产出）。 */
  plans: PlannedFile[];
  /** 每个 token 取一次下载链接（10 分钟有效，所以必须「现取现下」）。 */
  urlFor: (token: string) => Promise<string>;
  /** 并发数，默认 3（宿主上限 5）。 */
  concurrency?: number;
  onProgress?: (p: DownloadProgress) => void;
  /** 取消信号。 */
  shouldStop?: () => boolean;
}

/**
 * 按计划下载全部附件。
 *
 * 三条纪律（都来自实测教训）：
 *   1. **现取现下**：附件 URL 只有效 10 分钟，所以取链接与下载紧挨着做，不预先批量取。
 *   2. **失败不静默**：单个文件失败记录在 failures 里并继续，最后统一汇报。
 *   3. **顺序可预测**：按计划的顺序推进，方便"下到一半失败"时人工接着来。
 */
export async function downloadAll(params: DownloadParams): Promise<DownloadOutcome> {
  const { plans, urlFor, onProgress, shouldStop } = params;
  const concurrency = Math.max(1, Math.min(params.concurrency ?? 3, 5));
  const files: DownloadedFile[] = [];
  const failures: DownloadFailure[] = [];
  let done = 0;
  let cursor = 0;

  const report = (current?: string) => {
    onProgress?.({
      done,
      total: plans.length,
      ...(current ? { current } : {}),
    });
  };

  async function worker(): Promise<void> {
    for (;;) {
      if (shouldStop?.()) return;
      const idx = cursor;
      cursor += 1;
      const plan = plans[idx];
      if (!plan) return;
      report(plan.name);
      try {
        const url = await urlFor(plan.token);
        const data = await fetchBytes(url);
        files.push({ plan, data });
      } catch (err) {
        failures.push({
          plan,
          error: err instanceof Error ? err.message : String(err),
        });
      } finally {
        done += 1;
        report();
      }
    }
  }

  report();
  await Promise.all(Array.from({ length: concurrency }, () => worker()));
  return { files, failures };
}
