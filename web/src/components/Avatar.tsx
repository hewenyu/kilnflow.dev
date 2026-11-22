import { useState } from 'react'
import { avatarHue, avatarURL, tokenActor } from '../lib/format'
import css from './Avatar.module.css'

/**
 * Avatar 显示登录名的 GitHub 头像；加载不了、admin token 和 Bot 显示首字母（底色由登录名决定）。
 * 头像只是装饰，旁边总有登录名。
 */
export function Avatar({ login, size = 20 }: { login: string; size?: number }) {
  // 记录加载失败的那个地址：src 变了（换了账号）时自动重新尝试
  const [failedSrc, setFailedSrc] = useState<string | null>(null)
  const src = avatarURL(login, size <= 20 ? 40 : 96)
  const initial = Array.from(login.trim())[0]?.toUpperCase() ?? '?'
  return (
    <span
      className={`${css.av} ${login === tokenActor ? css.token : ''}`}
      style={{ width: size, height: size, fontSize: Math.round(size * 0.5), backgroundColor: `hsl(${avatarHue(login)} 45% 45%)` }}
      aria-hidden
    >
      {initial}
      {src && failedSrc !== src && <img src={src} alt="" loading="lazy" referrerPolicy="no-referrer" onError={() => setFailedSrc(src)} />}
    </span>
  )
}
