-- secret 的密文改为绑定作用域和名字（AAD，见 internal/secretbox）。旧密文没有绑定，无法再解密，全部清除；
-- 需要在升级后重新设置（kiln secret set）。
DELETE FROM secrets;
