package config

// 这个空导入不是多余的，删掉会让全新部署直接起不来。
//
// 原因：本包的各个 init() 会调用 facades.Config()，而这一步会实例化框架的
// 配置对象并校验 APP_KEY。APP_KEY 缺失或长度不是 32 时框架会 os.Exit(0)，
// 也就是 **main() 根本执行不到** —— 任何写在 main 里的「补 .env」准备都
// 永远没机会跑，全新解压的发布包只会打印一行 "Please initialize APP_KEY
// first." 然后退出，初始化向导页自然也无从访问。
//
// Go 保证被导入包的 init 先于导入包执行，所以 setup 的 init（补齐 APP_KEY /
// JWT_SECRET、记录数据库文件是否存在）一定发生在上面那些 init 之前。
//
// 注意：不要把这里的导入换成在 main() 里调用 setup.Prepare()——顺序不对。
import (
	_ "smart-mzcmc/app/setup"
)
