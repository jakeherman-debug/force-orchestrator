import { Controller, Get, Post, Put, Delete, Param, Body } from '@nestjs/common';
import { CreateUserDto } from './dto/create-user.dto';
import { UpdateUserDto } from './dto/update-user.dto';

@Controller('api/v1')
export class UserController {
  @Get('users')
  async findAll() {
    return [];
  }

  @Get('users/:id')
  async findOne(@Param('id') id: string) {
    return { id };
  }

  @Post('users')
  async create(@Body() dto: CreateUserDto) {
    return dto;
  }

  @Put('users/:id')
  async update(@Param('id') id: string, @Body() dto: UpdateUserDto) {
    return { id, ...dto };
  }

  @Delete('users/:id')
  async remove(@Param('id') id: string) {
    return { deleted: id };
  }
}

@Controller('health')
export class HealthController {
  @Get()
  check() {
    return { status: 'ok' };
  }
}
