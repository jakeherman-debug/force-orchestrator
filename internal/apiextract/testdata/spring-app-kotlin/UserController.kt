package com.example.api

import org.springframework.http.ResponseEntity
import org.springframework.web.bind.annotation.*

@RestController
@RequestMapping("/api/v1")
class UserController(private val userService: UserService) {

    @GetMapping("/users/{id}")
    fun getUser(@PathVariable id: Long): ResponseEntity<User> {
        return ResponseEntity.ok(userService.findById(id))
    }

    @PostMapping("/users")
    fun createUser(@RequestBody user: User): ResponseEntity<User> {
        return ResponseEntity.ok(userService.create(user))
    }

    @PutMapping("/users/{id}")
    fun updateUser(@PathVariable id: Long, @RequestBody user: User): ResponseEntity<User> {
        return ResponseEntity.ok(userService.update(id, user))
    }

    @DeleteMapping("/users/{id}")
    fun deleteUser(@PathVariable id: Long) {
        userService.delete(id)
    }

    @RequestMapping(value = "/search", method = [RequestMethod.GET])
    fun search(@RequestParam q: String): List<User> {
        return userService.search(q)
    }
}
