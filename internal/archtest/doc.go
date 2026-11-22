// Package archtest 检查 CI 与 AI 的领域边界（PLAN #30.3）：依赖测试（go list -deps，领域之间没有直接或传递依赖）和表归属
// 测试（各包 SQL 中出现的表属于允许的领域）。本包只有测试；新增的包、新建的表要先在 archtest_test.go 中归类。
package archtest
