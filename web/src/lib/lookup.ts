/**
 * lookup 按 key 查表 table（中文名称、Label 颜色等）：只取 table 自己的属性，没有时返回 undefined。
 *
 * key 常常来自服务端（状态、节点上报的阶段、Agent 写的报告），可以是任意字符串：直接写 `table[key]` 时 `__proto__`、
 * `constructor`、`toString` 等会取到对象原型上的值，React 渲染对象或函数时抛错、整页无法显示。用服务端给的值查表都用它。
 */
export function lookup<T>(table: Readonly<Record<string, T>>, key: string): T | undefined {
  return Object.hasOwn(table, key) ? table[key] : undefined
}
