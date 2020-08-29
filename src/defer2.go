package main

import "fmt"

type Car struct {
	model string
}

func (c *Car) PrintModel() {
	fmt.Println(c.model)
}

func main() {
	c := Car{model: "DeLorean DMC-12"}

	//defer c.PrintModel()


	c.model = "Chevrolet Impala"
	c.PrintModel()
	model(&c)
}

func model(c *Car) {
	fmt.Println(c.model)
}