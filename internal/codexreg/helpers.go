package codexreg

import (
	cryptorand "crypto/rand"
	"math/big"
	"strconv"
)

// 名字池尽量大一些：池子太小时成千上万个账号只会撞出两百多种姓名组合，本身就是一种批量特征。
var firstNames = []string{
	"Alex", "Jamie", "Taylor", "Jordan", "Casey", "Morgan", "Riley", "Avery", "Quinn", "Parker",
	"Cameron", "Reese", "Skyler", "Drew", "Emerson", "Liam", "Noah", "Oliver", "Ethan", "Lucas",
	"Mason", "Logan", "James", "Henry", "Owen", "Daniel", "Jack", "Samuel", "Nathan", "Caleb",
	"Emma", "Olivia", "Ava", "Sophia", "Mia", "Chloe", "Ella", "Grace", "Lily", "Hannah",
	"Zoe", "Nora", "Leah", "Maya", "Claire", "Julia", "Sarah", "Anna", "Lucy", "Ruby",
	"Evan", "Adam", "Ryan", "Kyle", "Blake", "Cole", "Dylan", "Aaron", "Luke", "Isaac",
}
var lastNames = []string{
	"Ray", "Lee", "Cole", "Reed", "Hunt", "Ford", "Shaw", "Gray", "Vance", "Wolfe",
	"Brooks", "Hayes", "Pierce", "Quinn", "Sloan", "Smith", "Johnson", "Brown", "Miller", "Davis",
	"Wilson", "Moore", "Clark", "Lewis", "Walker", "Hall", "Allen", "Young", "King", "Wright",
	"Scott", "Green", "Baker", "Adams", "Nelson", "Carter", "Mitchell", "Turner", "Parker", "Collins",
	"Edwards", "Stewart", "Morris", "Murphy", "Cook", "Rogers", "Morgan", "Cooper", "Peterson", "Bailey",
	"Bennett", "Foster", "Howard", "Ward", "Cox", "Hughes", "Price", "Bell", "Kelly", "Sanders",
}

func ri(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// genName 随机英文姓名。
func genName() string {
	return firstNames[ri(len(firstNames))] + " " + lastNames[ri(len(lastNames))]
}

// genAge 随机成年年龄（18-45）。
func genAge() string {
	return strconv.Itoa(18 + ri(28))
}

// GenPassword 生成满足强度要求（大小写+数字）的随机密码，供 producer 复用。
func GenPassword(n int) string {
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digit = "23456789"
	all := lower + upper + digit
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	b[0] = upper[ri(len(upper))]
	b[1] = lower[ri(len(lower))]
	b[2] = digit[ri(len(digit))]
	for i := 3; i < n; i++ {
		b[i] = all[ri(len(all))]
	}
	return string(b)
}
