// 把外部字符串（路径、节点名称、PATH）写进另一种语法时的转义规则，每种语法只在这里定义一次：
// POSIX shell（给 Agent 的工具命令）、systemd 单元文件（systemd.unit(5)、systemd.service(5)、systemd.exec(5)）、
// launchd 的 plist（XML 1.0）。

/** shellSafe 是不用引起来的词：不以 = 或 ~ 开头（zsh 的 =命令 展开、波浪号展开）。 */
const shellSafe = /^[\w./:@%+-][\w./:@%+=-]*$/;

/**
 * shellWord 把 s 写成 POSIX shell 的一个词（sh、bash、zsh、fish 都按字面理解）：只有安全字符时原样，否则用单引号引起来
 * （单引号中除了单引号本身都是字面的：$、反引号、反斜杠、空白、换行都不展开；单引号写成 '\''）。空串写成 ''。
 */
export function shellWord(s: string): string {
  return shellSafe.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;
}

// 下面几条规则要处理的正是控制字符（no-control-regex 防的是误写）。
/** controlChars 是 C0 控制字符和 DEL（单元文件中换行会结束这一行，XML 注释中不该有）。只用于 replace（带 g）。 */
// eslint-disable-next-line no-control-regex
const controlChars = /[\x00-\x1f\x7f]/g;

/** sdSpecifiers 让 s 中的 % 不被当成 specifier（systemd.unit(5) "Specifiers"：%% 是字面的 %）。 */
function sdSpecifiers(s: string): string {
  return s.replace(/%/g, '%%');
}

/**
 * sdQuoted 把 s 写成单元文件中用双引号引起来的一项（ExecStart= 的参数、Environment= 的赋值共用的规则）：反斜杠、双引号
 * 用 C 风格转义，控制字符写成 \xNN（引号中不能有换行），% 写成 %%。引号内的空白、单引号都是字面的。
 */
function sdQuoted(s: string): string {
  const escaped = s.replace(/[\\"]/g, '\\$&').replace(controlChars, (c) => `\\x${c.charCodeAt(0).toString(16).padStart(2, '0')}`);
  return `"${sdSpecifiers(escaped)}"`;
}

/** sdExecUnsafe 是 systemd 不接受出现在可执行文件路径中的字符（实测 systemd 252：Executable name contains special characters）。 */
// eslint-disable-next-line no-control-regex
const sdExecUnsafe = /["'\\\x00-\x1f\x7f]/;

/**
 * sdExecStart 把命令行写成 ExecStart= 的值（systemd.service(5) "Command lines"）：每一项按 sdQuoted 引起来；参数中的 $ 写成
 * $$（参数中的 $VAR、${VAR} 会被替换成环境变量）。可执行文件的路径不做变量替换，$ 原样写（写成 $$ 时 systemd 去找字面的
 * $$）；systemd 不接受其中有引号、反斜杠、控制字符，这时抛出。
 */
export function sdExecStart(program: string, args: readonly string[]): string {
  if (sdExecUnsafe.test(program)) throw new Error(`systemd 不接受可执行文件的路径中有引号、反斜杠或控制字符：${JSON.stringify(program)}（把 node 装到别的路径后再安装）`);
  return [sdQuoted(program), ...args.map((a) => sdQuoted(a).replace(/\$/g, '$$$$'))].join(' ');
}

/** sdEnvironment 是一条 Environment= 的值（systemd.exec(5)：赋值可以整个用引号引起来；$ 在这里没有特殊含义，不转义）。 */
export function sdEnvironment(name: string, value: string): string {
  return sdQuoted(`${name}=${value}`);
}

/**
 * sdText 是 Description= 这类自由文本（不去引号、不做 C 转义，只展开 specifier）：% 写成 %%，控制字符换成空格。调用方
 * 保证它不在行尾（行尾的反斜杠会把下一行接上）。
 */
export function sdText(s: string): string {
  return sdSpecifiers(s.replace(controlChars, ' '));
}

/** xmlInvalid 是 XML 1.0 中不能出现的字符（字符引用也不行）：制表、换行、回车以外的 C0 控制字符。 */
// eslint-disable-next-line no-control-regex
const xmlInvalid = /[\x00-\x08\x0b\x0c\x0e-\x1f]/;

/**
 * xmlText 把 s 写成 XML 的文本（plist 的 <string>）：& < > " ' 写成实体；制表、换行、回车写成字符引用（XML 解析时原样
 * 还原，不会被换行规范化改掉）。XML 1.0 不能表示的控制字符抛出（what 写进错误）。
 */
export function xmlText(s: string, what: string): string {
  if (xmlInvalid.test(s)) throw new Error(`${what} 中有 plist（XML）不能表示的控制字符：${JSON.stringify(s)}`);
  const entities: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;', '\t': '&#9;', '\n': '&#10;', '\r': '&#13;' };
  return s.replace(/[&<>"'\t\n\r]/g, (c) => entities[c] ?? c);
}

/** xmlComment 是放进 XML 注释（<!-- … -->）的说明文字：控制字符换成空格，不出现 "--"、不以 "-" 结尾。 */
export function xmlComment(s: string): string {
  return s
    .replace(controlChars, ' ')
    .replace(/-(?=-)/g, '- ')
    .replace(/-$/, '- ');
}
