plugins {
    java
    kotlin("jvm") version "1.9.21"
}

repositories {
    mavenCentral()
}

dependencies {
    implementation("org.springframework.boot:spring-boot-starter-web:3.2.0")
    implementation("com.fasterxml.jackson.core:jackson-databind:2.16.0")
    testImplementation("org.junit.jupiter:junit-jupiter:5.10.1")
}
