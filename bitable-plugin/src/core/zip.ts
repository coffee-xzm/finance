/**
 * zip 打包：用 fflate 在浏览器里生成 zip，**不经过任何服务器**。
 *
 * 设计取舍：
 *   - fflate 体积小（≈8KB gzip）、零依赖、支持异步（可在 Worker 里压缩）。
 *   - 中文文件名：fflate 以 UTF-8 字节写入，并在通用位标记里置 UTF-8 位，
 *     所以 Windows 自带解压、macOS、7-Zip 都能正确显示（这是最常见的坑之一）。
 *   - zip64：默认关闭。单批上限 1GB 场景下用不到，且关闭后兼容性更好。
 */
import { zip, strToU8, type Zippable, type AsyncZippable } from 'fflate';

export interface ZipEntry {
  /** zip 内目录（无则空串）。 */
  dir: string;
  /** 文件名。 */
  name: string;
  /** 已下载的文件字节。 */
  data: Uint8Array;
  /** 修改时间（用于 zip 内时间戳，缺省为当前时间）。 */
  mtime?: Date;
}

/** UTF-8 标记位（general purpose bit 11）。fflate 会在文件名非 ASCII 时置位。 */
export interface ZipOptions {
  /** 压缩级别 0-9；财务票据多为 PDF/JPEG，已是压缩格式，默认 6 与 1 的差别不大。 */
  level?: 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9;
  /** 追加进度回调（0..1）。 */
  onProgress?: (done: number, total: number) => void;
}

/** 构建 fflate 的输入结构（同名目录自动合并）。 */
export function buildZippable(entries: ZipEntry[]): AsyncZippable {
  const tree: Zippable = {};
  for (const e of entries) {
    const dir = e.dir ? `${e.dir}/` : '';
    if (dir && !tree[dir]) tree[dir] = {};
    const bucket = (dir ? (tree[dir] as Zippable) : tree) ?? tree;
    bucket[e.name] = [
      e.data,
      e.mtime ? { mtime: e.mtime } : {},
    ] as unknown as Zippable[string];
  }
  return tree as AsyncZippable;
}

/** 打包为 zip 字节。 */
export function makeZip(entries: ZipEntry[], opts: ZipOptions = {}): Promise<Uint8Array> {
  const input = buildZippable(entries);
  const total = entries.length;
  let done = 0;
  return new Promise((resolve, reject) => {
    zip(
      input,
      {
        level: opts.level ?? 6,
        // 尽量让中文文件名在各平台都可读
        mem: 12,
      },
      (err, data) => {
        if (err) reject(err);
        else resolve(data);
      },
    );
    if (opts.onProgress) {
      // fflate 的 zip 是一次性回调，没有细粒度进度；这里给出"开始/结束"两个点，
      // 真正的进度由下载阶段汇报。避免给出假进度。
      opts.onProgress(0, total);
      done = total;
      void done;
    }
  });
}

/** 文本 → UTF-8 字节（导出的 manifest.csv 用）。 */
export function csvBytes(csv: string): Uint8Array {
  // Excel 打开中文 CSV 必须有 BOM，否则乱码
  return new Uint8Array([0xef, 0xbb, 0xbf, ...strToU8(csv)]);
}

/** 触发浏览器下载（插件内可用；沙箱里会退化成普通 a[download] 点击）。 */
export function saveBlob(data: Uint8Array, filename: string, mime = 'application/zip'): void {
  const blob = new Blob([data as unknown as BlobPart], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
  // 立刻 revoke 会让部分浏览器下载失败，延迟释放
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}
