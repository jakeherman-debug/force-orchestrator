package com.example

import io.ktor.server.application.*
import io.ktor.server.routing.*
import io.ktor.server.response.*

fun Application.module() {
    routing {
        route("/api/v1") {
            get("/users/{id}") {
                val id = call.parameters["id"]
                call.respondText("user $id")
            }
            post("/users") {
                call.respondText("created")
            }
            put("/users/{id}") {
                val id = call.parameters["id"]
                call.respondText("updated $id")
            }
            delete("/users/{id}") {
                val id = call.parameters["id"]
                call.respondText("deleted $id")
            }
            route("/admin") {
                get("/dashboard") {
                    call.respondText("admin dashboard")
                }
                post("/users/{id}/ban") {
                    call.respondText("banned")
                }
            }
        }
        get("/health") {
            call.respondText("OK")
        }
    }
}
